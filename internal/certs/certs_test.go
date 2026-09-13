package certs

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const name = "api.ipeye.ru"

func pool(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	pem, err := os.ReadFile(filepath.Join(dir, caCrtFile))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		t.Fatal("ca.crt is not a certificate the trust store would take")
	}
	return p
}

// The property that matters, stated the way the camera experiences it: a
// client that trusts nothing but ca.crt completes a handshake with the
// balancer, by name. Everything else in this file is a way for that to fail
// less mysteriously.
func TestACameraTrustingTheCACanConnect(t *testing.T) {
	dir := t.TempDir()
	cert, err := Ensure(dir, name)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool(t, dir),
		// httptest listens on 127.0.0.1; the camera reaches the name.
		ServerName: name,
	}}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("handshake by name against the harness CA: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Re-minting on every start would invalidate the bundle already installed on
// the camera, so a second run has to hand back what the first one issued.
func TestASecondRunReusesWhatTheFirstIssued(t *testing.T) {
	dir := t.TempDir()
	first, err := Ensure(dir, name)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	firstCA, err := os.ReadFile(filepath.Join(dir, caCrtFile))
	if err != nil {
		t.Fatal(err)
	}

	second, err := Ensure(dir, name)
	if err != nil {
		t.Fatalf("Ensure again: %v", err)
	}
	if string(first.Certificate[0]) != string(second.Certificate[0]) {
		t.Error("the server certificate was reissued when it did not need to be")
	}
	secondCA, err := os.ReadFile(filepath.Join(dir, caCrtFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(firstCA) != string(secondCA) {
		t.Error("the CA was replaced, so every camera already set up is now broken")
	}
}

// -balancer-name can change between runs. Serving the old name would fail the
// handshake in a way that looks like the camera's fault.
func TestANewNameIsReissued(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, name); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cert, err := Ensure(dir, "cloud.example")
	if err != nil {
		t.Fatalf("Ensure for a new name: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("cloud.example"); err != nil {
		t.Errorf("certificate is not valid for the name asked for: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool(t, dir), DNSName: "cloud.example",
	}); err != nil {
		t.Errorf("reissued certificate does not chain to the CA on disk: %v", err)
	}
}

// Deleting the CA is how somebody starts over. A server certificate left
// behind from the old one is still in date and still for the right name, so
// only the signature says it is worthless.
func TestARemovedCAReissuesTheServerCertificate(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, name); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, f := range []string{caCrtFile, caKeyFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}

	cert, err := Ensure(dir, name)
	if err != nil {
		t.Fatalf("Ensure after removing the CA: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool(t, dir), DNSName: name,
	}); err != nil {
		t.Errorf("server certificate still chains to the CA that was deleted: %v", err)
	}
}

func TestTheKeysAreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, name); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, f := range []string{caKeyFile, serverKeyFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is mode %04o", f, mode)
		}
	}
}

func TestAnEmptyNameIsRefused(t *testing.T) {
	if _, err := Ensure(t.TempDir(), ""); err == nil {
		t.Error("Ensure accepted an empty name")
	}
}
