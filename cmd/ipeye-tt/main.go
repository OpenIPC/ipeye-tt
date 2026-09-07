// ipeye-tt stands in for a vdk-based cloud so a camera's reverse RTSP
// connection can be measured on a bench. See README.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/OpenIPC/ipeye-tt/internal/balancer"
	"github.com/OpenIPC/ipeye-tt/internal/bridge"
	"github.com/OpenIPC/ipeye-tt/internal/certs"
	"github.com/OpenIPC/ipeye-tt/internal/logx"
	"github.com/OpenIPC/ipeye-tt/internal/probe"
	"github.com/OpenIPC/ipeye-tt/internal/report"
)

func main() {
	var (
		mode       = flag.String("mode", "reverse", "reverse: the camera dials in (fake balancer + listener); forward: dial the camera's RTSP port")
		advertise  = flag.String("advertise", "", "reverse: IP the camera can reach this host at (required)")
		rtspPort   = flag.Int("rtsp-port", 8554, "reverse: port the camera is told to dial")
		httpsPort  = flag.Int("https-port", 443, "reverse: fake balancer HTTPS port (0 = off)")
		httpPort   = flag.Int("http-port", 0, "reverse: fake balancer plain HTTP port for older builds (0 = off)")
		name       = flag.String("balancer-name", "api.ipeye.ru", "reverse: name the certificate is issued for; point the camera at it via /etc/hosts")
		certDir    = flag.String("cert-dir", "certs", "reverse: where ca.crt/server.crt are kept (generated when missing)")
		path       = flag.String("path", "/mpeg4", "reverse: RTSP path the cloud client asks for")
		fwdURL     = flag.String("url", "", "forward: rtsp://user:pass@camera:554/stream=0")
		duration   = flag.Duration("duration", 5*time.Minute, "how long to run before the verdict")
		rwTimeout  = flag.Duration("rw-timeout", 10*time.Second, "vdk ReadWriteTimeout: the cloud gives up after this much silence")
		keepalive  = flag.Duration("keepalive", 10*time.Second, "send the cloud keepalive (CRLFCRLF, expects 'ok') this often; 0 = never")
		kaGlued    = flag.Bool("keepalive-glued", false, "send the keepalive in the same write as the next RTSP request")
		throttle   = flag.Int("throttle", 0, "read from the camera at most this many kbit/s (0 = unlimited)")
		throttleAt = flag.Duration("throttle-after", 10*time.Second, "start throttling this long after PLAY")
		stallFor   = flag.Duration("stall", 0, "stop reading from the camera for this long (0 = never)")
		stallAt    = flag.Duration("stall-after", 20*time.Second, "start the stall this long after PLAY")
		noAudio    = flag.Bool("no-audio", false, "do not SETUP the audio track")
		maxGap     = flag.Duration("max-gap", 2*time.Second, "verdict: longest tolerated silence on a track")
		maxDrift   = flag.Duration("max-drift-per-min", 500*time.Millisecond, "verdict: tolerated vdk audio-vs-video timeline drift per minute")
		expectSess = flag.Int("expect-sessions", 1, "verdict: more sessions than this means reconnects")
		jsonOut    = flag.String("json", "", "write the full record here")
		traceOut   = flag.String("trace", "", "write a per-frame CSV (session,t,ch,len,pt,seq,ts,marker) here")
		verbose    = flag.Bool("v", false, "log every RTSP request and vdk debug line")
		cloudUser  = flag.String("cloud-user", "ipeye", "credentials the cloud presents when a claimed camera asks (any non-empty pair is accepted by the IPEYE callback)")
		cloudPass  = flag.String("cloud-pass", "ipeye", "password paired with -cloud-user")
	)
	flag.Parse()
	probe.Install(*verbose)
	bridge.SetVerbose(*verbose)

	impaired := *throttle > 0 || *stallFor > 0
	b := &bridge.Bridge{Verbose: *verbose, CloudUser: *cloudUser, CloudPass: *cloudPass, Cond: bridge.Conditions{
		KeepaliveEvery: *keepalive, KeepaliveGlued: *kaGlued, ThrottleKbps: *throttle, ThrottleAfter: *throttleAt,
		StallAfter: *stallAt, StallFor: *stallFor, ReadWriteTimeout: *rwTimeout, DialTimeout: 5 * time.Second, DisableAudio: *noAudio,
	}}
	if *traceOut != "" {
		f, err := os.Create(*traceOut)
		if err != nil {
			logx.L.Fatal(err)
		}
		defer f.Close()
		fmt.Fprintln(f, "session,t,ch,len,pt,seq,ts,marker")
		bridge.SetTrace(f)
	}
	run := &report.Run{Tool: "ipeye-tt", Mode: *mode, Started: time.Now(), Config: map[string]any{
		"advertise": *advertise, "rtsp_port": *rtspPort, "https_port": *httpsPort, "http_port": *httpPort, "url": *fwdURL,
		"duration": duration.String(), "rw_timeout": rwTimeout.String(), "keepalive": keepalive.String(), "keepalive_glued": *kaGlued,
		"throttle_kbps": *throttle, "throttle_after": throttleAt.String(), "stall": stallFor.String(), "stall_after": stallAt.String(), "no_audio": *noAudio,
	}}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; logx.L.Print("interrupted, evaluating"); cancel() }()

	var bal *balancer.Balancer
	switch *mode {
	case "reverse":
		if *advertise == "" {
			logx.L.Fatal("-advertise is required in reverse mode")
		}
		cert, err := certs.Ensure(*certDir, *name)
		if err != nil {
			logx.Fatalf("certificates: %v", err)
		}
		logx.Printf("CA for the camera's trust bundle: %s/ca.crt (issued for %s)", *certDir, *name)
		bal = &balancer.Balancer{Advertise: fmt.Sprintf("%s:%d", *advertise, *rtspPort)}
		bal.OnLookup = func(l balancer.Lookup) {
			logx.Printf("[balancer] %s %s?%s tls=%v ua=%q → %s", l.Remote, l.Path, l.Query, l.TLS, l.UserAgent, bal.Advertise)
		}
		httpsAddr, httpAddr := "", ""
		if *httpsPort > 0 {
			httpsAddr = fmt.Sprintf(":%d", *httpsPort)
		}
		if *httpPort > 0 {
			httpAddr = fmt.Sprintf(":%d", *httpPort)
		}
		if err := bal.Start(httpsAddr, httpAddr, cert); err != nil {
			logx.L.Fatal(err)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *rtspPort))
		if err != nil {
			logx.Fatalf("reverse listen: %v", err)
		}
		logx.Printf("waiting for cameras on %s (balancer https:%d http:%d), run length %s", ln.Addr(), *httpsPort, *httpPort, *duration)
		b.ServeReverse(ctx, ln, *path)
	case "forward":
		if *fwdURL == "" {
			logx.L.Fatal("-url is required in forward mode")
		}
		logx.Printf("dialling %s, run length %s", *fwdURL, *duration)
		b.DialForward(ctx, *fwdURL, time.Second)
	default:
		logx.Fatalf("unknown mode %q", *mode)
	}
	b.Wait()
	if bal != nil {
		run.Lookups = bal.Count()
		bal.Stop()
	}
	run.Ended = time.Now()
	run.Sessions = b.Sessions()
	report.Evaluate(run, report.Thresholds{MaxGap: *maxGap, ExpectSessions: *expectSess, AudioRateTol: 0.05, MaxDriftPerMin: *maxDrift, MinVideoSeconds: 5, Impaired: impaired})
	report.WriteText(os.Stdout, run)
	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err != nil {
			logx.L.Fatal(err)
		}
		if err := report.WriteJSON(f, run); err != nil {
			logx.L.Fatal(err)
		}
		f.Close()
	}
	if !run.Pass {
		os.Exit(1)
	}
}
