// Package probe drives the stock vdk RTSP client the way a vdk-based cloud
// would: dial, consume every packet it demuxes, and watch its signals. It
// records the timelines vdk produces, since that is what the cloud's muxer
// works from.
package probe

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/OpenIPC/ipeye-tt/internal/logx"
	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/format/rtspv2"
)

// Options configure the vdk client.
type Options struct {
	URL              string
	DialTimeout      time.Duration
	ReadWriteTimeout time.Duration
	DisableAudio     bool
}

// Result is what vdk saw over one session.
type Result struct {
	DialError       string   `json:"dial_error,omitempty"`
	Codecs          []string `json:"codecs"`
	SDPRaw          string   `json:"-"`
	VideoPackets    int64    `json:"video_packets"`
	VideoKeyframes  int64    `json:"video_keyframes"`
	AudioPackets    int64    `json:"audio_packets"`
	VideoTime0Ms    int64    `json:"video_time0_ms"`
	VideoTimeMs     int64    `json:"video_time_ms"` // last video Time - first
	VideoTimeBack   int64    `json:"video_time_backwards"`
	AudioTimeMs     int64    `json:"audio_time_ms"` // last audio Time - first
	DriftMs         int64    `json:"av_drift_ms"`   // audio timeline - video timeline at the end
	MaxAbsDriftMs   int64    `json:"max_abs_drift_ms"`
	WallMs          int64    `json:"wall_ms"` // wall clock between first and last packet
	VideoVsWallMs   int64    `json:"video_vs_wall_ms"`
	StopSignal      bool     `json:"stop_signal"`
	StopReason      string   `json:"stop_reason,omitempty"`
	InitOffsetMs    int64    `json:"av_init_offset_ms"`
	DriftSlopeMsMin float64  `json:"av_drift_slope_ms_min"`
	VdkLog          []string `json:"vdk_log"`
	firstPkt        time.Time
	lastPkt         time.Time
	haveV, haveA    bool
	v0, vLast       time.Duration
	a0, aLast       time.Duration
	drift           time.Duration
	firstBoth       time.Time
	settleAt        time.Time
	driftAtSettle   time.Duration
	haveSettle      bool
}

// capture receives vdk's debug output, which goes through Go's global
// logger. One global buffer; each session remembers where it started and
// reads its own lines back from there.
type capture struct {
	mu    sync.Mutex
	lines []string
	echo  bool
}

var vdkLog = &capture{}

// Install routes the global logger into the capture. With echo, every vdk
// line is also printed to stderr, request echoes flattened to one line.
func Install(echo bool) {
	vdkLog.echo = echo
	log.SetOutput(vdkLog)
	log.SetFlags(0)
}

func (c *capture) Write(p []byte) (int, error) {
	s := strings.TrimSpace(string(p))
	c.mu.Lock()
	c.lines = append(c.lines, s)
	if len(c.lines) > 5000 {
		c.lines = c.lines[len(c.lines)-2500:]
	}
	c.mu.Unlock()
	if c.echo {
		logx.Printf("[vdk] %s", strings.ReplaceAll(s, "\r\n", "\\r\\n"))
	}
	return len(p), nil
}

func (c *capture) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

// since returns the lines recorded after mark and the last one that is not
// a request echo — the reason vdk gave up, when it did.
func (c *capture) since(mark int) ([]string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if mark > len(c.lines) {
		mark = 0
	}
	out := append([]string(nil), c.lines[mark:]...)
	last := ""
	for _, l := range out {
		if !strings.Contains(l, "RTSP/1.0\r\n") && !strings.Contains(l, "CSeq:") {
			last = l
		}
	}
	if len(out) > 200 {
		out = out[len(out)-200:]
	}
	return out, last
}

// Run dials and consumes until the client stops or ctx is done.
func Run(ctx context.Context, o Options) *Result {
	r := &Result{}
	mark := vdkLog.mark()
	client, err := rtspv2.Dial(rtspv2.RTSPClientOptions{URL: o.URL, DialTimeout: o.DialTimeout, ReadWriteTimeout: o.ReadWriteTimeout, DisableAudio: o.DisableAudio, Debug: true})
	if err != nil {
		r.DialError = err.Error()
		r.VdkLog, _ = vdkLog.since(mark)
		return r
	}
	defer client.Close()
	r.SDPRaw = string(client.SDPRaw)
	videoIdx, audioIdx := -1, -2
	for i, c := range client.CodecData {
		r.Codecs = append(r.Codecs, c.Type().String())
		switch c.(type) {
		case av.VideoCodecData:
			if videoIdx < 0 {
				videoIdx = i
			}
		case av.AudioCodecData:
			if audioIdx < 0 {
				audioIdx = i
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			r.finish(mark)
			return r
		case sig := <-client.Signals:
			if sig == rtspv2.SignalStreamRTPStop {
				r.StopSignal = true
			}
			r.finish(mark)
			return r
		case pkt := <-client.OutgoingPacketQueue:
			r.packet(pkt, videoIdx, audioIdx)
		}
	}
}

func (r *Result) packet(p *av.Packet, videoIdx, audioIdx int) {
	now := time.Now()
	if r.firstPkt.IsZero() {
		r.firstPkt = now
	}
	r.lastPkt = now
	switch int(p.Idx) {
	case videoIdx:
		r.VideoPackets++
		if p.IsKeyFrame {
			r.VideoKeyframes++
		}
		if !r.haveV {
			r.v0, r.haveV = p.Time, true
			r.VideoTime0Ms = p.Time.Milliseconds()
		} else if p.Time < r.vLast {
			r.VideoTimeBack++
		}
		r.vLast = p.Time
	case audioIdx:
		r.AudioPackets++
		if !r.haveA {
			r.a0, r.haveA = p.Time, true
		}
		r.aLast = p.Time
		if r.haveV {
			d := (r.aLast - r.a0) - (r.vLast - r.v0)
			r.drift = d
			if r.firstBoth.IsZero() {
				r.firstBoth = now
				r.InitOffsetMs = d.Milliseconds()
			}
			// Ignore the first 5 s: startup fills jitter buffers unevenly and
			// that shows up as a fixed offset, not the growing drift a
			// clock-rate mismatch causes.
			if !r.haveSettle && now.Sub(r.firstBoth) >= 5*time.Second {
				r.haveSettle, r.settleAt, r.driftAtSettle = true, now, d
			}
			ad := d
			if ad < 0 {
				ad = -ad
			}
			if ad.Milliseconds() > r.MaxAbsDriftMs {
				r.MaxAbsDriftMs = ad.Milliseconds()
			}
		}
	}
}

func (r *Result) finish(mark int) {
	r.VdkLog, r.StopReason = vdkLog.since(mark)
	if r.haveV {
		r.VideoTimeMs = (r.vLast - r.v0).Milliseconds()
	}
	if r.haveA {
		r.AudioTimeMs = (r.aLast - r.a0).Milliseconds()
	}
	r.DriftMs = r.drift.Milliseconds()
	if r.haveSettle {
		if secs := r.lastPkt.Sub(r.settleAt).Seconds(); secs > 1 {
			r.DriftSlopeMsMin = float64((r.drift - r.driftAtSettle).Milliseconds()) / secs * 60
		}
	}
	if !r.firstPkt.IsZero() {
		r.WallMs = r.lastPkt.Sub(r.firstPkt).Milliseconds()
		r.VideoVsWallMs = r.VideoTimeMs - r.WallMs
	}
}

// String is a one-line summary.
func (r *Result) String() string {
	if r.DialError != "" {
		return "dial: " + r.DialError
	}
	return fmt.Sprintf("codecs=%v video=%d(key %d) audio=%d vtime=%dms atime=%dms drift=%dms(init %d, slope %.0fms/min, max %d) wall=%dms stop=%v reason=%q",
		r.Codecs, r.VideoPackets, r.VideoKeyframes, r.AudioPackets, r.VideoTimeMs, r.AudioTimeMs, r.DriftMs, r.InitOffsetMs, r.DriftSlopeMsMin, r.MaxAbsDriftMs, r.WallMs, r.StopSignal, r.StopReason)
}
