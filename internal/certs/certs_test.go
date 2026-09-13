package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

// os.WriteFile's mode applies only when it creates the file, so a key that
// already existed with permissive bits kept them through a reissue — private
// signing material left readable by every local user, with Ensure reporting
// success.
func TestAPermissiveKeyIsTightened(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, name); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("%s is mode %04o", caKeyFile, mode)
	}
}

// A CA whose key does not belong to its certificate parses perfectly and signs
// nothing a client will accept. Carrying on with it produces a handshake
// failure that reads as a camera fault.
func TestAMismatchedCAIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, name); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Someone else's key, over the top of a valid certificate.
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, caKeyFile),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}

	cert, err := Ensure(dir, name)
	if err != nil {
		t.Fatalf("Ensure with a mismatched CA: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool(t, dir), DNSName: name,
	}); err != nil {
		t.Errorf("served certificate does not chain to the CA on disk: %v", err)
	}
}

// Not valid *yet* fails a handshake as completely as expired, and a host clock
// that was ahead when a certificate was written leaves exactly that behind.
func TestALeafThatIsNotValidYetIsReissued(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, name); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	caCert, caKey, err := loadCA(dir)
	if err != nil {
		t.Fatalf("loadCA: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	future := &x509.Certificate{
		SerialNumber: big.NewInt(99),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(48 * time.Hour),
		NotAfter:     time.Now().Add(72 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, future, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := write(dir, serverCrtFile, serverKeyFile, der, key); err != nil {
		t.Fatal(err)
	}

	cert, err := Ensure(dir, name)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.NotBefore.After(time.Now()) {
		t.Error("a certificate that is not valid yet was served anyway")
	}
}

// Two instances started together share the default -cert-dir. Whichever loses
// the race must not go on serving a leaf that chains to a root no longer on
// disk: cameras trust the file, so that instance would simply be unreachable
// and the run would blame the camera.
func TestConcurrentStartsAgreeOnOneRoot(t *testing.T) {
	dir := t.TempDir()

	const n = 8
	var wg sync.WaitGroup
	leaves := make([]tls.Certificate, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leaves[i], errs[i] = Ensure(dir, name)
		}(i)
	}
	wg.Wait()

	final := pool(t, dir)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Ensure %d: %v", i, errs[i])
		}
		leaf, err := x509.ParseCertificate(leaves[i].Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: final, DNSName: name,
		}); err != nil {
			t.Errorf("start %d serves a certificate the published CA does not cover: %v", i, err)
		}
	}
}
