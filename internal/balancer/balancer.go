// Package balancer is the fake IPEYE balancer: the HTTPS endpoint a camera asks
// for the address of a cloud node before it dials out. It answers every
// lookup with the address of this harness and logs what the camera sent.
package balancer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/OpenIPC/ipeye-tt/internal/logx"
)

// Lookup is one balancer request as the camera made it.
type Lookup struct {
	T         time.Time `json:"t"`
	Remote    string    `json:"remote"`
	Path      string    `json:"path"`
	Query     string    `json:"query"`
	UserAgent string    `json:"user_agent"`
	TLS       bool      `json:"tls"`
}

// Balancer serves the lookup endpoint over HTTPS (and optionally plain HTTP,
// for older builds that used port 8111) and hands every camera the same
// reverse-RTSP address.
type Balancer struct {
	Advertise string // host:port the camera should dial for reverse RTSP
	OnLookup  func(Lookup)
	count     atomic.Int64
	servers   []*http.Server
}

// Count is how many lookups were answered.
func (b *Balancer) Count() int64 { return b.count.Load() }

func (b *Balancer) handler(tlsOn bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := Lookup{T: time.Now(), Remote: r.RemoteAddr, Path: r.URL.Path, Query: r.URL.RawQuery, UserAgent: r.UserAgent(), TLS: tlsOn}
		b.count.Add(1)
		if b.OnLookup != nil {
			b.OnLookup(l)
		}
		if !strings.HasPrefix(r.URL.Path, "/balancer/server/") {
			http.NotFound(w, r)
			return
		}
		// The camera splits the message on '|' and takes a literal IPv4 on the
		// left and the digits on the right.
		host, port, _ := net.SplitHostPort(b.Advertise)
		body, _ := json.Marshal(map[string]string{"code": "200", "message": fmt.Sprintf("%s|%s", host, port)})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

// Start listens on the given ports (0 disables one). The TLS certificate must
// be issued for the name the camera dials.
func (b *Balancer) Start(httpsAddr, httpAddr string, cert tls.Certificate) error {
	if httpsAddr != "" {
		srv := &http.Server{
			Addr:      httpsAddr,
			Handler:   b.handler(true),
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
			ErrorLog:  log.New(logWriter{"balancer-https"}, "", 0),
		}
		ln, err := net.Listen("tcp", httpsAddr)
		if err != nil {
			return fmt.Errorf("balancer https listen %s: %w", httpsAddr, err)
		}
		go func() { _ = srv.ServeTLS(ln, "", "") }()
		b.servers = append(b.servers, srv)
	}
	if httpAddr != "" {
		srv := &http.Server{Addr: httpAddr, Handler: b.handler(false), ErrorLog: log.New(logWriter{"balancer-http"}, "", 0)}
		ln, err := net.Listen("tcp", httpAddr)
		if err != nil {
			return fmt.Errorf("balancer http listen %s: %w", httpAddr, err)
		}
		go func() { _ = srv.Serve(ln) }()
		b.servers = append(b.servers, srv)
	}
	return nil
}

// Stop shuts the listeners down.
func (b *Balancer) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, s := range b.servers {
		_ = s.Shutdown(ctx)
	}
}

type logWriter struct{ tag string }

func (l logWriter) Write(p []byte) (int, error) {
	logx.Printf("[%s] %s", l.tag, strings.TrimSpace(string(p)))
	return len(p), nil
}
