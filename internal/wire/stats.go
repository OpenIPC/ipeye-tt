package wire

import (
	"fmt"
	"sync"
	"time"
)

// TrackStats accumulates what one interleaved RTP channel carried.
type TrackStats struct {
	Ch         int     `json:"ch"`
	Kind       string  `json:"kind"`
	Codec      string  `json:"codec"`
	Clock      int     `json:"clock"`
	Frames     int64   `json:"frames"`
	Bytes      int64   `json:"bytes"`
	Payload    int64   `json:"payload_bytes"`
	Markers    int64   `json:"markers"`
	FirstTS    uint32  `json:"first_ts"`
	LastTS     uint32  `json:"last_ts"`
	TSAdvance  int64   `json:"ts_advance"` // sum of signed ts deltas
	TSBack     int64   `json:"ts_backwards"`
	TSWraps    int64   `json:"ts_wraps"`
	SeqLost    int64   `json:"seq_lost"`
	SeqBack    int64   `json:"seq_backwards"`
	MaxGapMs   float64 `json:"max_gap_ms"`
	MaxGapAt   string  `json:"max_gap_at,omitempty"`
	Seconds    float64 `json:"seconds"`
	FirstAt    string  `json:"first_at,omitempty"`
	RTCPSR     int64   `json:"rtcp_sr"`
	RTCPSDES   int64   `json:"rtcp_sdes"`
	RTCPBye    int64   `json:"rtcp_bye"`
	RTCPOther  int64   `json:"rtcp_other"`
	firstT     time.Time
	lastT      time.Time
	lastSeq    uint16
	haveSeq    bool
	haveTS     bool
	prevTS     uint32
	extTSBase  int64
	PayloadBps float64 `json:"payload_bytes_per_s"`
	// TSPerPayload is RTP ticks per payload byte: 1.0 for G.711 at the
	// advertised clock; 2.0 means the payload carries half the samples the
	// timestamps claim.
	TSPerPayload float64 `json:"ts_per_payload_byte"`
}

// Stats collects everything seen on one session.
type Stats struct {
	mu          sync.Mutex
	Tracks      map[int]*TrackStats `json:"tracks"`
	Responses   int                 `json:"responses"`
	Keepalives  int                 `json:"keepalive_acks"`
	Desyncs     []Desync            `json:"desyncs"`
	LastRxAt    time.Time           `json:"-"`
	MaxRxGapMs  float64             `json:"max_rx_gap_ms"`
	MaxRxGapAt  string              `json:"max_rx_gap_at,omitempty"`
	SDP         []Media             `json:"sdp"`
	SDPRaw      string              `json:"sdp_raw,omitempty"`
	StatusLines []string            `json:"status_lines"`
	started     time.Time
}

// NewStats returns an empty collector.
func NewStats(started time.Time) *Stats {
	return &Stats{Tracks: map[int]*TrackStats{}, started: started}
}

func (s *Stats) rel(t time.Time) string { return fmt.Sprintf("%.3fs", t.Sub(s.started).Seconds()) }

// MapChannel binds an RTP channel to a media section (from SETUP).
func (s *Stats) MapChannel(ch int, m Media) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.track(ch)
	t.Kind, t.Codec, t.Clock = m.Kind, m.Codec, m.Clock
	s.track(ch + 1).Kind = m.Kind + "-rtcp"
}

func (s *Stats) track(ch int) *TrackStats {
	t := s.Tracks[ch]
	if t == nil {
		t = &TrackStats{Ch: ch}
		s.Tracks[ch] = t
	}
	return t
}

// RxBytes notes that the camera sent something (any bytes) at t.
func (s *Stats) RxBytes(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.LastRxAt.IsZero() {
		if g := t.Sub(s.LastRxAt).Seconds() * 1000; g > s.MaxRxGapMs {
			s.MaxRxGapMs, s.MaxRxGapAt = g, s.rel(t)
		}
	}
	s.LastRxAt = t
}

// OnFrame implements Handler.
func (s *Stats) OnFrame(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.track(f.Ch)
	t.Frames++
	t.Bytes += int64(f.Len)
	if t.firstT.IsZero() {
		t.firstT = f.T
		t.FirstAt = s.rel(f.T)
	} else if g := f.T.Sub(t.lastT).Seconds() * 1000; g > t.MaxGapMs {
		t.MaxGapMs, t.MaxGapAt = g, s.rel(f.T)
	}
	t.lastT = f.T
	t.Seconds = t.lastT.Sub(t.firstT).Seconds()
	if f.Ch%2 == 1 {
		switch f.RTCP {
		case 200:
			t.RTCPSR++
		case 202:
			t.RTCPSDES++
		case 203:
			t.RTCPBye++
		default:
			t.RTCPOther++
		}
		return
	}
	r := f.RTP
	if r == nil {
		return
	}
	t.Payload += int64(r.Payload)
	if r.Marker {
		t.Markers++
	}
	if t.haveSeq {
		d := int(r.Seq) - int(t.lastSeq)
		if d < -32768 {
			d += 65536
		} else if d > 32768 {
			d -= 65536
		}
		if d > 1 {
			t.SeqLost += int64(d - 1)
		} else if d < 1 {
			t.SeqBack++
		}
	}
	t.lastSeq, t.haveSeq = r.Seq, true
	if !t.haveTS {
		t.FirstTS, t.prevTS, t.haveTS = r.TS, r.TS, true
	} else if r.TS != t.prevTS {
		d := int64(int32(r.TS - t.prevTS))
		if d < 0 {
			t.TSBack++
		}
		if r.TS < t.prevTS && d > 0 {
			t.TSWraps++
		}
		t.TSAdvance += d
		t.prevTS = r.TS
	}
	t.LastTS = r.TS
	if t.Seconds > 0 {
		t.PayloadBps = float64(t.Payload) / t.Seconds
	}
	if t.Payload > 0 {
		t.TSPerPayload = float64(t.TSAdvance) / float64(t.Payload)
	}
}

// OnResponse implements Handler.
func (s *Stats) OnResponse(r Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Responses++
	s.StatusLines = append(s.StatusLines, s.rel(r.T)+" "+r.Status)
	if r.Headers["content-type"] == "application/sdp" && r.Body != "" {
		s.SDPRaw = r.Body
		s.SDP = ParseSDP(r.Body)
	}
}

// OnKeepaliveAck implements Handler.
func (s *Stats) OnKeepaliveAck(time.Time) {
	s.mu.Lock()
	s.Keepalives++
	s.mu.Unlock()
}

// OnDesync implements Handler.
func (s *Stats) OnDesync(d Desync) {
	s.mu.Lock()
	if len(s.Desyncs) < 50 {
		s.Desyncs = append(s.Desyncs, d)
	}
	s.mu.Unlock()
}

// Snapshot returns a copy safe to marshal.
func (s *Stats) Snapshot() *Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Stats{Tracks: map[int]*TrackStats{}, Responses: s.Responses, Keepalives: s.Keepalives, Desyncs: append([]Desync(nil), s.Desyncs...), MaxRxGapMs: s.MaxRxGapMs, MaxRxGapAt: s.MaxRxGapAt, SDP: s.SDP, SDPRaw: s.SDPRaw, StatusLines: append([]string(nil), s.StatusLines...), started: s.started}
	for k, v := range s.Tracks {
		cp := *v
		c.Tracks[k] = &cp
	}
	return c
}

// Track returns the stats of the first RTP channel of the given kind, or nil.
func (s *Stats) Track(kind string) *TrackStats {
	for ch := 0; ch < 64; ch += 2 {
		if t, ok := s.Tracks[ch]; ok && t.Kind == kind {
			return t
		}
	}
	return nil
}
