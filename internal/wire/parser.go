// Package wire parses what a camera sends down an RTSP-over-TCP session —
// interleaved RTP/RTCP frames, RTSP responses, and the IPEYE keepalive
// acknowledgement — and keeps per-track statistics. The parser is fed the raw
// byte stream and returns the bytes that should reach the cloud client, which
// is everything except the keepalive acknowledgement (the real cloud swallows
// that before its RTSP client sees it).
package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RTP is the fixed header of one RTP packet.
type RTP struct {
	PT      uint8
	Marker  bool
	Seq     uint16
	TS      uint32
	SSRC    uint32
	Payload int // payload bytes after header, CSRCs, extension and padding
}

// Frame is one interleaved frame ($ ch len payload).
type Frame struct {
	T    time.Time
	Ch   int
	Len  int
	RTP  *RTP // nil for RTCP or unparseable
	RTCP int  // RTCP packet type when the channel is odd, else 0
}

// Response is one RTSP response from the camera.
type Response struct {
	T       time.Time
	Status  string
	Headers map[string]string
	Body    string
}

// Desync is a run of bytes that is neither a frame, a response nor an ack.
type Desync struct {
	T       time.Time
	Skipped int
	Hex     string
	ASCII   string
}

// Handler receives parsed events.
type Handler interface {
	OnFrame(Frame)
	OnResponse(Response)
	OnKeepaliveAck(time.Time)
	OnDesync(Desync)
}

// Parser is a streaming parser over the camera→cloud byte stream.
type Parser struct {
	mu        sync.Mutex
	buf       []byte
	h         Handler
	ackWanted int
	inDesync  bool
	MaxBuffer int
}

// New returns a parser delivering events to h.
func New(h Handler) *Parser { return &Parser{h: h, MaxBuffer: 1 << 20} }

// ExpectAck tells the parser one keepalive has been sent and an "ok" may follow.
func (p *Parser) ExpectAck() {
	p.mu.Lock()
	p.ackWanted++
	p.mu.Unlock()
}

// Feed appends b and returns the bytes to forward to the cloud client.
func (p *Parser) Feed(b []byte, now time.Time) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	var out []byte
	for len(p.buf) > 0 {
		c := p.buf[0]
		switch {
		case c == '$':
			if len(p.buf) < 4 {
				return out
			}
			n := int(binary.BigEndian.Uint16(p.buf[2:4]))
			if len(p.buf) < 4+n {
				return out
			}
			f := Frame{T: now, Ch: int(p.buf[1]), Len: n}
			if f.Ch%2 == 0 {
				f.RTP = parseRTP(p.buf[4 : 4+n])
			} else if n >= 2 {
				f.RTCP = int(p.buf[5])
			}
			p.inDesync = false
			p.h.OnFrame(f)
			out = append(out, p.buf[:4+n]...)
			p.buf = p.buf[4+n:]
		case c == 'R':
			if len(p.buf) < 5 {
				return out
			}
			if !bytes.HasPrefix(p.buf, []byte("RTSP/")) {
				out = p.desync(out, now)
				continue
			}
			i := bytes.Index(p.buf, []byte("\r\n\r\n"))
			if i < 0 {
				if len(p.buf) > 16384 {
					out = p.desync(out, now)
					continue
				}
				return out
			}
			head := string(p.buf[:i])
			hdrs := map[string]string{}
			lines := strings.Split(head, "\r\n")
			for _, l := range lines[1:] {
				if k, v, ok := strings.Cut(l, ":"); ok {
					hdrs[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
				}
			}
			cl, _ := strconv.Atoi(hdrs["content-length"])
			if len(p.buf) < i+4+cl {
				return out
			}
			p.inDesync = false
			p.h.OnResponse(Response{T: now, Status: lines[0], Headers: hdrs, Body: string(p.buf[i+4 : i+4+cl])})
			out = append(out, p.buf[:i+4+cl]...)
			p.buf = p.buf[i+4+cl:]
		case c == 'o':
			if len(p.buf) < 2 {
				return out
			}
			if p.buf[1] == 'k' && p.ackWanted > 0 {
				p.ackWanted--
				p.inDesync = false
				p.h.OnKeepaliveAck(now)
				p.buf = p.buf[2:]
				continue
			}
			out = p.desync(out, now)
		default:
			out = p.desync(out, now)
		}
	}
	return out
}

// desync records the garbage run once and forwards bytes up to the next thing
// that looks like a frame or a response, faithfully: the cloud client sees
// exactly what the camera sent.
func (p *Parser) desync(out []byte, now time.Time) []byte {
	j := -1
	for k := 1; k < len(p.buf); k++ {
		if p.buf[k] == '$' || bytes.HasPrefix(p.buf[k:], []byte("RTSP/")) {
			j = k
			break
		}
	}
	if j < 0 {
		if len(p.buf) < 5 && !p.inDesync {
			// might be a split "RTSP" or "ok"; wait for more
			return out
		}
		j = len(p.buf)
	}
	if !p.inDesync {
		n := j
		if n > 64 {
			n = 64
		}
		p.h.OnDesync(Desync{T: now, Skipped: j, Hex: hex.EncodeToString(p.buf[:n]), ASCII: printable(p.buf[:n])})
		p.inDesync = true
	}
	out = append(out, p.buf[:j]...)
	p.buf = p.buf[j:]
	return out
}

func printable(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 32 && c < 127 {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('.')
		}
	}
	return sb.String()
}

func parseRTP(b []byte) *RTP {
	if len(b) < 12 || b[0]>>6 != 2 {
		return nil
	}
	r := &RTP{PT: b[1] & 0x7f, Marker: b[1]&0x80 != 0, Seq: binary.BigEndian.Uint16(b[2:4]), TS: binary.BigEndian.Uint32(b[4:8]), SSRC: binary.BigEndian.Uint32(b[8:12])}
	off := 12 + 4*int(b[0]&0x0f)
	end := len(b)
	if b[0]&0x10 != 0 && end >= off+4 {
		off += 4 + 4*int(binary.BigEndian.Uint16(b[off+2:off+4]))
	}
	if b[0]&0x20 != 0 && end > off {
		end -= int(b[end-1])
	}
	if end < off {
		return r
	}
	r.Payload = end - off
	return r
}
