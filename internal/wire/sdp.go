package wire

import (
	"strconv"
	"strings"
)

// Media is one m= section of the camera's SDP as far as the harness cares.
type Media struct {
	Kind    string // video / audio
	PT      int
	Codec   string // H264, H265, PCMA, ...
	Clock   int    // rtpmap clock rate
	Control string // a=control value
}

// ParseSDP extracts the media sections in order.
func ParseSDP(s string) []Media {
	var out []Media
	var cur *Media
	for _, raw := range strings.Split(s, "\n") {
		l := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(l, "m="):
			f := strings.Fields(l[2:])
			m := Media{Kind: f[0]}
			if len(f) >= 4 {
				m.PT, _ = strconv.Atoi(f[3])
			}
			out = append(out, m)
			cur = &out[len(out)-1]
		case cur != nil && strings.HasPrefix(l, "a=rtpmap:"):
			// a=rtpmap:8 PCMA/8000/1
			rest := l[len("a=rtpmap:"):]
			pt, spec, _ := strings.Cut(rest, " ")
			if n, _ := strconv.Atoi(pt); n == cur.PT {
				parts := strings.Split(spec, "/")
				cur.Codec = strings.ToUpper(parts[0])
				if len(parts) > 1 {
					cur.Clock, _ = strconv.Atoi(parts[1])
				}
			}
		case cur != nil && strings.HasPrefix(l, "a=control:"):
			cur.Control = l[len("a=control:"):]
		}
	}
	return out
}

// Interleaved returns the RTP channel from a Transport header, or -1.
func Interleaved(transport string) int {
	for _, p := range strings.Split(transport, ";") {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "interleaved=") {
			v := p[len("interleaved="):]
			a, _, _ := strings.Cut(v, "-")
			if n, err := strconv.Atoi(a); err == nil {
				return n
			}
		}
	}
	return -1
}
