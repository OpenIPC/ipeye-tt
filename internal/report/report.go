// Package report turns a run's sessions into a verdict: JSON for machines,
// a short table for people, and a non-zero exit when the camera did not
// behave like a stream a cloud could keep up with.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/OpenIPC/ipeye-tt/internal/bridge"
	"github.com/OpenIPC/ipeye-tt/internal/wire"
)

// Thresholds decide the verdict.
type Thresholds struct {
	MaxGap          time.Duration // camera silent for longer than this
	ExpectSessions  int           // more sessions than this = reconnects happened
	AudioRateTol    float64       // |payload rate / clock - 1| tolerance for G.711
	MaxDriftPerMin  time.Duration // vdk audio timeline vs video timeline
	MinVideoSeconds float64       // a session shorter than this proves nothing
	Impaired        bool          // a throttle or stall was applied: reconnects are not failures by themselves
}

// Run is the whole record.
type Run struct {
	Tool     string            `json:"tool"`
	Mode     string            `json:"mode"`
	Started  time.Time         `json:"started"`
	Ended    time.Time         `json:"ended"`
	Config   map[string]any    `json:"config"`
	Lookups  int64             `json:"balancer_lookups"`
	Sessions []*bridge.Session `json:"sessions"`
	Pass     bool              `json:"pass"`
	Findings []string          `json:"findings"`
	Summary  []string          `json:"summary"`
}

// Evaluate fills Pass/Findings/Summary.
func Evaluate(r *Run, th Thresholds) {
	var f, sum []string
	fail := func(format string, a ...any) { f = append(f, fmt.Sprintf(format, a...)) }
	note := func(format string, a ...any) { sum = append(sum, fmt.Sprintf(format, a...)) }

	finished := 0
	for _, s := range r.Sessions {
		if !s.Ended.IsZero() {
			finished++
		}
	}
	note("sessions: %d (%d finished), balancer lookups: %d", len(r.Sessions), finished, r.Lookups)
	if len(r.Sessions) == 0 {
		fail("no session: the camera never connected")
	}
	if !th.Impaired && th.ExpectSessions > 0 && len(r.Sessions) > th.ExpectSessions {
		fail("%d sessions where %d expected: the camera reconnected %d time(s)", len(r.Sessions), th.ExpectSessions, len(r.Sessions)-th.ExpectSessions)
	}
	for _, s := range r.Sessions {
		w := s.Wire
		if w == nil {
			continue
		}
		v, a := w.Track("video"), w.Track("audio")
		head := fmt.Sprintf("session %d (%.1fs, ended by %s %s)", s.ID, s.Seconds, s.EndedBy, s.EndErr)
		if s.Vdk != nil {
			head += " vdk: " + s.Vdk.String()
		}
		note(head)
		if v != nil {
			note("  video ch%d %s/%d: %d frames %d B, %.0f kbit/s, max gap %.0f ms at %s, ts back %d wraps %d, seq lost %d, markers %d, rtcp sr %d",
				v.Ch, v.Codec, v.Clock, v.Frames, v.Bytes, float64(v.Bytes)*8/1000/math.Max(v.Seconds, 0.001), v.MaxGapMs, v.MaxGapAt, v.TSBack, v.TSWraps, v.SeqLost, v.Markers, rtcp(w, v.Ch+1))
		}
		if a != nil {
			note("  audio ch%d %s/%d: %d frames, payload %.0f B/s (clock %d → ratio %.3f), ts/payload %.3f, max gap %.0f ms, ts back %d, seq lost %d, rtcp sr %d",
				a.Ch, a.Codec, a.Clock, a.Frames, a.PayloadBps, a.Clock, ratio(a.PayloadBps, float64(a.Clock)), a.TSPerPayload, a.MaxGapMs, a.TSBack, a.SeqLost, rtcp(w, a.Ch+1))
		}
		if v != nil && a != nil && v.Clock > 0 && a.Clock > 0 {
			note("  first ts: video %d (%.3f s @%d), audio %d (%.3f s @%d), difference %.3f s",
				v.FirstTS, float64(v.FirstTS)/float64(v.Clock), v.Clock, a.FirstTS, float64(a.FirstTS)/float64(a.Clock), a.Clock,
				float64(v.FirstTS)/float64(v.Clock)-float64(a.FirstTS)/float64(a.Clock))
		}
		note("  rx: max silence %.0f ms at %s; responses %d; keepalives sent %d acked %d; desyncs %d", w.MaxRxGapMs, w.MaxRxGapAt, w.Responses, s.Keepalive, w.Keepalives, len(w.Desyncs))
		for _, d := range w.Desyncs {
			note("    desync at %.3fs: %d bytes %q", d.T.Sub(s.Started).Seconds(), d.Skipped, d.ASCII)
		}

		// Checks.
		if len(w.Desyncs) > 0 {
			fail("session %d: %d desync run(s) — bytes that are neither an interleaved frame nor an RTSP response (first: %q)", s.ID, len(w.Desyncs), w.Desyncs[0].ASCII)
		}
		if s.Keepalive > w.Keepalives && !th.Impaired {
			fail("session %d: %d keepalive(s) sent, %d acknowledged", s.ID, s.Keepalive, w.Keepalives)
		}
		if v == nil {
			if s.Seconds >= th.MinVideoSeconds {
				fail("session %d: no video track was set up", s.ID)
			}
			continue
		}
		if v.Seconds < th.MinVideoSeconds {
			continue
		}
		if !th.Impaired && th.MaxGap > 0 && time.Duration(v.MaxGapMs*float64(time.Millisecond)) > th.MaxGap {
			fail("session %d: video silent for %.0f ms at %s (limit %s)", s.ID, v.MaxGapMs, v.MaxGapAt, th.MaxGap)
		}
		if v.TSBack > 0 {
			fail("session %d: video RTP timestamp went backwards %d time(s)", s.ID, v.TSBack)
		}
		if v.SeqLost > 0 {
			fail("session %d: %d video RTP packet(s) missing by sequence number on a TCP session", s.ID, v.SeqLost)
		}
		if a != nil && a.Seconds >= th.MinVideoSeconds {
			if isG711(a.Codec) && a.Clock > 0 {
				if d := math.Abs(ratio(a.PayloadBps, float64(a.Clock)) - 1); d > th.AudioRateTol {
					fail("session %d: audio %s payload runs at %.0f B/s against an advertised clock of %d Hz (ratio %.3f; G.711 must be 1 byte per tick)", s.ID, a.Codec, a.PayloadBps, a.Clock, ratio(a.PayloadBps, float64(a.Clock)))
				}
				if a.TSPerPayload > 0 && math.Abs(a.TSPerPayload-1) > th.AudioRateTol {
					fail("session %d: audio RTP timestamps advance %.3f ticks per payload byte (G.711 must be 1.000)", s.ID, a.TSPerPayload)
				}
			}
			if !th.Impaired && th.MaxGap > 0 && time.Duration(a.MaxGapMs*float64(time.Millisecond)) > th.MaxGap {
				fail("session %d: audio silent for %.0f ms at %s (limit %s)", s.ID, a.MaxGapMs, a.MaxGapAt, th.MaxGap)
			}
			if a.SeqLost > 0 {
				fail("session %d: %d audio RTP packet(s) missing by sequence number on a TCP session", s.ID, a.SeqLost)
			}
		}
		if s.Vdk != nil && s.Vdk.DialError == "" {
			if s.Vdk.StopSignal && !th.Impaired && s.EndedBy != "ctx" {
				fail("session %d: vdk stopped the stream: %s", s.ID, s.Vdk.StopReason)
			}
			if th.MaxDriftPerMin > 0 && s.Vdk.AudioPackets > 0 && math.Abs(s.Vdk.DriftSlopeMsMin) > float64(th.MaxDriftPerMin.Milliseconds()) {
				fail("session %d: vdk audio timeline drifts %.0f ms/min against video after settling (init offset %d ms; limit %s)", s.ID, s.Vdk.DriftSlopeMsMin, s.Vdk.InitOffsetMs, th.MaxDriftPerMin)
			}
			if s.Vdk.VideoTimeBack > 0 {
				fail("session %d: vdk video packet time went backwards %d time(s)", s.ID, s.Vdk.VideoTimeBack)
			}
		}
	}
	r.Findings, r.Summary, r.Pass = f, sum, len(f) == 0
}

func isG711(c string) bool { return c == "PCMA" || c == "PCMU" }

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func rtcp(w *wire.Stats, ch int) int64 {
	if t, ok := w.Tracks[ch]; ok {
		return t.RTCPSR
	}
	return 0
}

// WriteJSON writes the run.
func WriteJSON(w io.Writer, r *Run) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the human summary and the verdict.
func WriteText(w io.Writer, r *Run) {
	fmt.Fprintf(w, "ipeye-tt %s run %s → %s\n", r.Mode, r.Started.Format(time.RFC3339), r.Ended.Format(time.RFC3339))
	for _, l := range r.Summary {
		fmt.Fprintln(w, l)
	}
	if r.Pass {
		fmt.Fprintln(w, "VERDICT: PASS")
		return
	}
	fmt.Fprintln(w, "VERDICT: FAIL")
	for _, l := range r.Findings {
		fmt.Fprintln(w, "  - "+strings.TrimSpace(l))
	}
}
