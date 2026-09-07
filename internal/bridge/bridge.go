// Package bridge splices a camera's RTSP socket to a stock vdk client and
// measures everything that crosses. In reverse mode the camera dials the
// bridge (after asking the fake balancer where to go) and announces itself
// with a REGISTER line; in forward mode the bridge dials the camera. Either
// way vdk then dials a loopback listener and the bridge copies bytes both
// ways, parsing the camera's side, injecting the cloud keepalive, and
// optionally throttling or stalling its own reads to model a slow uplink.
package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenIPC/ipeye-tt/internal/logx"
	"github.com/OpenIPC/ipeye-tt/internal/probe"
	"github.com/OpenIPC/ipeye-tt/internal/wire"
)

// Conditions are the impairments a run can apply to the cloud side.
type Conditions struct {
	KeepaliveEvery   time.Duration // 0 = none
	KeepaliveGlued   bool          // prepend the keepalive to the next RTSP request instead of a separate write
	ThrottleKbps     int           // 0 = none; caps how fast the cloud reads from the camera
	ThrottleAfter    time.Duration // start throttling this long after PLAY
	StallAfter       time.Duration // stop reading this long after PLAY
	StallFor         time.Duration // and for this long (0 = never)
	ReadWriteTimeout time.Duration
	DialTimeout      time.Duration
	DisableAudio     bool
}

// Request is one RTSP request the cloud sent.
type Request struct {
	At     string `json:"at"`
	Method string `json:"method"`
	URI    string `json:"uri"`
	CSeq   string `json:"cseq,omitempty"`
}

// Session is one camera connection from REGISTER (or dial) to close.
type Session struct {
	ID        int           `json:"id"`
	Started   time.Time     `json:"started"`
	Ended     time.Time     `json:"ended"`
	Seconds   float64       `json:"seconds"`
	Remote    string        `json:"remote"`
	Register  string        `json:"register,omitempty"`
	Requests  []Request     `json:"requests"`
	PlayAt    string        `json:"play_at,omitempty"`
	Keepalive int           `json:"keepalives_sent"`
	EndedBy   string        `json:"ended_by"`
	EndErr    string        `json:"end_error,omitempty"`
	Wire      *wire.Stats   `json:"wire"`
	Vdk       *probe.Result `json:"vdk"`
	stats     *wire.Stats
	parser    *wire.Parser
	mu        sync.Mutex
	playAt    time.Time
	glueKA    atomic.Int32
	cam       net.Conn
	cloud     net.Conn
	closeOnce sync.Once
	closeBy   string
	closeErr  error
	pending   []wire.Media // SETUP order, from the SDP
	setupCh   map[string]int
}

func (s *Session) rel(t time.Time) string { return fmt.Sprintf("%.3fs", t.Sub(s.Started).Seconds()) }

func (s *Session) closeAll(by string, err error) {
	s.closeOnce.Do(func() {
		s.closeBy, s.closeErr = by, err
		if s.cam != nil {
			_ = s.cam.Close()
		}
		if s.cloud != nil {
			_ = s.cloud.Close()
		}
	})
}

// Bridge runs sessions and keeps their records.
type Bridge struct {
	Cond      Conditions
	Verbose   bool
	CloudUser string
	CloudPass string
	Trace     io.Writer // per-frame CSV, may be nil
	mu        sync.Mutex
	sessions  []*Session
	next      int
	wg        sync.WaitGroup
}

// Sessions returns the sessions so far (finished ones have Ended set).
func (b *Bridge) Sessions() []*Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*Session(nil), b.sessions...)
}

// Wait blocks until every session goroutine has returned.
func (b *Bridge) Wait() { b.wg.Wait() }

func (b *Bridge) logf(format string, a ...any) { logx.Printf("[bridge] "+format, a...) }

// ServeReverse accepts cameras on ln until ctx ends.
func (b *Bridge) ServeReverse(ctx context.Context, ln net.Listener, path string) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.reverseSession(ctx, c, path)
		}()
	}
}

func (b *Bridge) reverseSession(ctx context.Context, cam net.Conn, path string) {
	s := b.newSession(cam.RemoteAddr().String())
	s.cam = cam
	_ = cam.SetReadDeadline(time.Now().Add(10 * time.Second))
	reg, rest, err := readRegister(cam)
	_ = cam.SetReadDeadline(time.Time{})
	if err != nil {
		b.logf("session %d: no REGISTER from %s: %v", s.ID, s.Remote, err)
		s.EndedBy, s.EndErr = "register", err.Error()
		s.finish()
		_ = cam.Close()
		return
	}
	s.Register = reg
	b.logf("session %d: %s registered: %s", s.ID, s.Remote, reg)
	b.run(ctx, s, rest, path)
}

// DialForward connects to the camera itself and runs one session; it
// redials after a pause until ctx ends, the way a cloud would.
func (b *Bridge) DialForward(ctx context.Context, rtspURL string, redial time.Duration) {
	u, err := url.Parse(rtspURL)
	if err != nil {
		b.logf("bad url %q: %v", rtspURL, err)
		return
	}
	host := u.Host
	if u.Port() == "" {
		host += ":554"
	}
	for ctx.Err() == nil {
		d := net.Dialer{Timeout: 5 * time.Second}
		cam, err := d.DialContext(ctx, "tcp", host)
		if err != nil {
			b.logf("dial %s: %v", host, err)
		} else {
			s := b.newSession(cam.RemoteAddr().String())
			s.cam = cam
			path := u.RequestURI()
			if u.User != nil {
				path = "//" + u.User.String() + "@" + path
			}
			b.wg.Add(1)
			func() {
				defer b.wg.Done()
				b.run(ctx, s, nil, path)
			}()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(redial):
		}
	}
}

func (b *Bridge) newSession(remote string) *Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.next++
	now := time.Now()
	s := &Session{ID: b.next, Started: now, Remote: remote, setupCh: map[string]int{}}
	s.stats = wire.NewStats(now)
	s.parser = wire.New(s)
	b.sessions = append(b.sessions, s)
	return s
}

func (s *Session) finish() {
	s.Ended = time.Now()
	s.Seconds = s.Ended.Sub(s.Started).Seconds()
	s.Wire = s.stats.Snapshot()
}

// run: loopback listener → vdk dials it → splice. `pre` is any camera bytes
// read past the REGISTER line.
func (b *Bridge) run(ctx context.Context, s *Session, pre []byte, path string) {
	lb, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.EndedBy, s.EndErr = "loopback", err.Error()
		s.finish()
		_ = s.cam.Close()
		return
	}
	// path may carry "//user:pass@" to hand vdk credentials in forward mode.
	target := "rtsp://" + lb.Addr().String() + path
	if strings.HasPrefix(path, "//") {
		creds, p, _ := strings.Cut(path[2:], "@")
		target = "rtsp://" + creds + "@" + lb.Addr().String() + p
	} else if b.CloudUser != "" {
		// A claimed camera answers 401; stock vdk only sends Basic auth after
		// one, and needs a non-empty user:pass to form it. The camera's IPEYE
		// callback accepts any pair, so these are placeholders that just have
		// to be well-formed — the real cloud sends its own.
		target = "rtsp://" + b.CloudUser + ":" + b.CloudPass + "@" + lb.Addr().String() + path
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var vdkDone = make(chan *probe.Result, 1)
	go func() {
		res := probe.Run(sctx, probe.Options{URL: target, DialTimeout: b.Cond.DialTimeout, ReadWriteTimeout: b.Cond.ReadWriteTimeout, DisableAudio: b.Cond.DisableAudio})
		// A cloud whose client gave up closes its socket; vdk leaves a failed
		// dial's connection open, so do it for it.
		if res.DialError != "" {
			s.closeAll("vdk-dial-failed", fmt.Errorf("%s", res.DialError))
		} else if res.StopSignal && sctx.Err() == nil {
			s.closeAll("vdk-stopped", fmt.Errorf("%s", res.StopReason))
		}
		vdkDone <- res
	}()
	_ = lb.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	cloud, err := lb.Accept()
	_ = lb.Close()
	if err != nil {
		s.EndedBy, s.EndErr = "vdk-dial", err.Error()
		cancel()
		s.Vdk = <-vdkDone
		s.finish()
		_ = s.cam.Close()
		return
	}
	s.cloud = cloud

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.camToCloud(s, pre) }()
	go func() { defer wg.Done(); b.cloudToCam(s) }()
	if b.Cond.KeepaliveEvery > 0 {
		go b.keepalive(sctx, s)
	}
	go func() { <-sctx.Done(); s.closeAll("ctx", nil) }()
	wg.Wait()
	cancel()
	s.Vdk = <-vdkDone
	s.EndedBy = s.closeBy
	if s.closeErr != nil {
		s.EndErr = s.closeErr.Error()
	}
	s.finish()
	b.logf("session %d ended after %.1fs by %s %s; vdk: %s", s.ID, s.Seconds, s.EndedBy, s.EndErr, s.Vdk)
}

func (b *Bridge) keepalive(ctx context.Context, s *Session) {
	t := time.NewTicker(b.Cond.KeepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if b.Cond.KeepaliveGlued {
				s.glueKA.Store(1)
				continue
			}
			s.parser.ExpectAck()
			s.mu.Lock()
			s.Keepalive++
			s.mu.Unlock()
			if _, err := s.cam.Write([]byte("\r\n\r\n")); err != nil {
				s.closeAll("keepalive-write", err)
				return
			}
		}
	}
}

// camToCloud reads the camera, parses, forwards to vdk, applying the
// throttle and stall conditions to its own reads.
func (b *Bridge) camToCloud(s *Session, pre []byte) {
	buf := make([]byte, 64*1024)
	var tokens float64
	last := time.Now()
	throttled, stalled := false, false
	feed := func(p []byte) bool {
		now := time.Now()
		s.stats.RxBytes(now)
		out := s.parser.Feed(p, now)
		if len(out) == 0 {
			return true
		}
		if _, err := s.cloud.Write(out); err != nil {
			s.closeAll("cloud-write", err)
			return false
		}
		return true
	}
	if len(pre) > 0 && !feed(pre) {
		return
	}
	for {
		s.mu.Lock()
		playAt := s.playAt
		s.mu.Unlock()
		if !playAt.IsZero() {
			since := time.Since(playAt)
			if b.Cond.StallFor > 0 && !stalled && since >= b.Cond.StallAfter {
				stalled = true
				b.logf("session %d: stall: not reading for %s", s.ID, b.Cond.StallFor)
				time.Sleep(b.Cond.StallFor)
				b.logf("session %d: stall over", s.ID)
				last = time.Now()
			}
			if b.Cond.ThrottleKbps > 0 && !throttled && since >= b.Cond.ThrottleAfter {
				throttled = true
				b.logf("session %d: throttle: reading at %d kbit/s", s.ID, b.Cond.ThrottleKbps)
				last = time.Now()
			}
		}
		want := len(buf)
		if throttled {
			rate := float64(b.Cond.ThrottleKbps) * 1000 / 8 // bytes per second
			now := time.Now()
			tokens += now.Sub(last).Seconds() * rate
			last = now
			if burst := rate * 0.2; tokens > burst {
				tokens = burst
			}
			if tokens < 1500 {
				time.Sleep(time.Duration(float64(time.Second) * (1500 - tokens) / rate))
				continue
			}
			want = int(tokens)
			if want > len(buf) {
				want = len(buf)
			}
		}
		n, err := s.cam.Read(buf[:want])
		if n > 0 {
			if throttled {
				tokens -= float64(n)
			}
			if !feed(buf[:n]) {
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				s.closeAll("camera-eof", nil)
			} else {
				s.closeAll("camera-read", err)
			}
			return
		}
	}
}

// cloudToCam reads vdk's requests, logs them, optionally glues the
// keepalive in front, and writes to the camera.
func (b *Bridge) cloudToCam(s *Session) {
	r := bufio.NewReader(s.cloud)
	for {
		req, err := readRequest(r)
		if len(req) > 0 {
			b.noteRequest(s, req)
			out := req
			if s.glueKA.Swap(0) == 1 {
				s.parser.ExpectAck()
				s.mu.Lock()
				s.Keepalive++
				s.mu.Unlock()
				out = append([]byte("\r\n\r\n"), req...)
			}
			if _, werr := s.cam.Write(out); werr != nil {
				s.closeAll("camera-write", werr)
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				s.closeAll("vdk-closed", nil)
			} else {
				s.closeAll("vdk-read", err)
			}
			return
		}
	}
}

// readRequest returns one RTSP request (header block; vdk sends no bodies).
func readRequest(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := r.ReadBytes('\n')
		out = append(out, line...)
		if err != nil {
			return out, err
		}
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			return out, nil
		}
	}
}

func (s *Session) noteRequest(req []byte) {
	head, _, _ := strings.Cut(string(req), "\r\n")
	f := strings.Fields(head)
	if len(f) < 2 {
		return
	}
	rq := Request{At: s.rel(time.Now()), Method: f[0], URI: f[1]}
	transport := ""
	for _, l := range strings.Split(string(req), "\r\n")[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "cseq":
			rq.CSeq = strings.TrimSpace(v)
		case "transport":
			transport = strings.TrimSpace(v)
		}
	}
	s.mu.Lock()
	s.Requests = append(s.Requests, rq)
	switch rq.Method {
	case "SETUP":
		if ch := wire.Interleaved(transport); ch >= 0 {
			s.setupCh[rq.URI] = ch
			if m := s.mediaFor(rq.URI); m != nil {
				s.stats.MapChannel(ch, *m)
			}
		}
	case "PLAY":
		if s.playAt.IsZero() {
			s.playAt = time.Now()
			s.PlayAt = rq.At
		}
	}
	s.mu.Unlock()
}

func (b *Bridge) noteRequest(s *Session, req []byte) {
	s.noteRequest(req)
	if b.Verbose {
		head, _, _ := strings.Cut(string(req), "\r\n")
		b.logf("session %d: cloud → %s", s.ID, head)
	}
}

// mediaFor finds the SDP section whose control matches the SETUP URI.
func (s *Session) mediaFor(uri string) *wire.Media {
	for i := range s.pending {
		m := &s.pending[i]
		if m.Control != "" && strings.HasSuffix(uri, m.Control) {
			return m
		}
	}
	// Fall back to SETUP order.
	n := len(s.setupCh) - 1
	if n >= 0 && n < len(s.pending) {
		return &s.pending[n]
	}
	return nil
}

// Handler plumbing: the session sits between the parser and the stats so it
// can see the SDP as it goes by.

func (s *Session) OnFrame(f wire.Frame) {
	s.stats.OnFrame(f)
	if s.Trace() != nil && f.RTP != nil {
		fmt.Fprintf(s.Trace(), "%d,%.6f,%d,%d,%d,%d,%d,%t\n", s.ID, f.T.Sub(s.Started).Seconds(), f.Ch, f.Len, f.RTP.PT, f.RTP.Seq, f.RTP.TS, f.RTP.Marker)
	}
}

func (s *Session) OnResponse(r wire.Response) {
	s.stats.OnResponse(r)
	if verbose {
		extra := ""
		for _, k := range []string{"www-authenticate", "session", "transport", "content-length"} {
			if v, ok := r.Headers[k]; ok {
				extra += " " + k + "=" + v
			}
		}
		logx.Printf("[bridge] session %d: camera ← %s%s", s.ID, r.Status, extra)
	}
	if r.Headers["content-type"] == "application/sdp" {
		s.mu.Lock()
		s.pending = wire.ParseSDP(r.Body)
		s.mu.Unlock()
	}
}

func (s *Session) OnKeepaliveAck(t time.Time) { s.stats.OnKeepaliveAck(t) }

func (s *Session) OnDesync(d wire.Desync) {
	s.stats.OnDesync(d)
	logx.Printf("[bridge] session %d: DESYNC at %s: %d bytes, hex=%s ascii=%q", s.ID, s.rel(d.T), d.Skipped, d.Hex, d.ASCII)
}

var traceWriter io.Writer

var verbose bool

// SetVerbose turns on per-request/response logging.
func SetVerbose(v bool) { verbose = v }

// SetTrace installs the per-frame CSV sink for all sessions.
func SetTrace(w io.Writer) { traceWriter = w }

// Trace returns the per-frame sink.
func (s *Session) Trace() io.Writer { return traceWriter }

// readRegister consumes "REGISTER={...}" (brace-balanced, no terminator) and
// returns the JSON plus any bytes that followed it.
func readRegister(c net.Conn) (string, []byte, error) {
	var acc []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		acc = append(acc, buf[:n]...)
		if i := bytes.Index(acc, []byte("REGISTER=")); i >= 0 {
			depth, start := 0, -1
			for j := i + len("REGISTER="); j < len(acc); j++ {
				switch acc[j] {
				case '{':
					if depth == 0 {
						start = j
					}
					depth++
				case '}':
					depth--
					if depth == 0 && start >= 0 {
						js := acc[start : j+1]
						var v map[string]any
						if json.Unmarshal(js, &v) != nil {
							return string(js), acc[j+1:], fmt.Errorf("REGISTER is not JSON")
						}
						return string(js), acc[j+1:], nil
					}
				}
			}
		}
		if err != nil {
			return string(acc), nil, err
		}
		if len(acc) > 65536 {
			return "", nil, fmt.Errorf("no REGISTER in first 64 KiB")
		}
	}
}
