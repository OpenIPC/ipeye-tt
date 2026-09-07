package wire

import (
	"encoding/binary"
	"testing"
	"time"
)

type collector struct {
	frames []Frame
	resp   []Response
	acks   int
	desync []Desync
}

func (c *collector) OnFrame(f Frame)          { c.frames = append(c.frames, f) }
func (c *collector) OnResponse(r Response)     { c.resp = append(c.resp, r) }
func (c *collector) OnKeepaliveAck(time.Time)  { c.acks++ }
func (c *collector) OnDesync(d Desync)         { c.desync = append(c.desync, d) }

func interleaved(ch int, payload []byte) []byte {
	b := []byte{'$', byte(ch), 0, 0}
	binary.BigEndian.PutUint16(b[2:4], uint16(len(payload)))
	return append(b, payload...)
}

func rtp(pt uint8, seq uint16, ts uint32, payloadLen int) []byte {
	b := make([]byte, 12+payloadLen)
	b[0] = 0x80
	b[1] = pt
	binary.BigEndian.PutUint16(b[2:4], seq)
	binary.BigEndian.PutUint32(b[4:8], ts)
	return b
}

func TestFrameAndResponseSplit(t *testing.T) {
	c := &collector{}
	p := New(c)
	stream := interleaved(0, rtp(96, 1, 1000, 20))
	stream = append(stream, []byte("RTSP/1.0 200 OK\r\nCSeq: 3\r\n\r\n")...)
	stream = append(stream, interleaved(2, rtp(8, 7, 8000, 160))...)
	// Feed one byte at a time: the parser must reassemble across reads.
	out := []byte{}
	for _, x := range stream {
		out = append(out, p.Feed([]byte{x}, time.Now())...)
	}
	if len(c.frames) != 2 {
		t.Fatalf("want 2 frames, got %d", len(c.frames))
	}
	if c.frames[0].Ch != 0 || c.frames[0].RTP == nil || c.frames[0].RTP.PT != 96 {
		t.Errorf("frame 0 wrong: %+v", c.frames[0])
	}
	if c.frames[1].RTP.Payload != 160 {
		t.Errorf("audio payload want 160, got %d", c.frames[1].RTP.Payload)
	}
	if len(c.resp) != 1 || c.resp[0].Headers["cseq"] != "3" {
		t.Errorf("response wrong: %+v", c.resp)
	}
	// Everything forwards verbatim.
	if len(out) != len(stream) {
		t.Errorf("forwarded %d bytes, fed %d", len(out), len(stream))
	}
}

func TestKeepaliveAck(t *testing.T) {
	c := &collector{}
	p := New(c)
	p.ExpectAck()
	out := p.Feed([]byte("ok"), time.Now())
	if c.acks != 1 {
		t.Fatalf("want 1 ack, got %d", c.acks)
	}
	if len(out) != 0 {
		t.Errorf("ack must not forward to the cloud, forwarded %d bytes", len(out))
	}
}

func TestDesyncOnStrayBytes(t *testing.T) {
	c := &collector{}
	p := New(c)
	// An RTSP error line injected into the media stream, then a valid frame.
	stream := append([]byte("RTSP/1.0 400 Bad Request\r\n\r\n"), interleaved(0, rtp(96, 1, 90, 12))...)
	// "RTSP/1.0 400..." is a well-formed response, not a desync — the real
	// hazard is a truncated/garbage run. Feed genuine garbage first.
	garbage := append([]byte{0x01, 0x02, 0x03, 0xff, 0x00}, stream...)
	p.Feed(garbage, time.Now())
	if len(c.desync) == 0 {
		t.Fatalf("garbage before a frame must be reported as desync")
	}
	if len(c.frames) != 1 {
		t.Errorf("want the trailing frame recovered, got %d frames", len(c.frames))
	}
}

func TestParseSDPRates(t *testing.T) {
	sdp := "m=video 0 RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=control:video\r\n" +
		"m=audio 0 RTP/AVP 8\r\na=rtpmap:8 PCMA/16000/1\r\na=control:audio\r\n"
	m := ParseSDP(sdp)
	if len(m) != 2 || m[0].Codec != "H264" || m[0].Clock != 90000 {
		t.Fatalf("video parse wrong: %+v", m)
	}
	if m[1].Codec != "PCMA" || m[1].Clock != 16000 || m[1].Control != "audio" {
		t.Errorf("audio parse wrong: %+v", m[1])
	}
}
