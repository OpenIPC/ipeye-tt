// The balancer's certificate authority, and the server certificate it issues.
//
// A camera reaches the balancer by name over TLS, so a stand-in has to be both
// resolvable as that name and trusted as it — an IP SAN does not help, because
// the camera is asking for a host. Ensure mints a CA once and keeps it, so the
// trust bundle a camera has already been given stays valid across runs;
// re-minting on every start would mean re-running scripts/cam-setup.sh every
// time.
//
// Nothing here is a credential for anything real. The CA is generated locally,
// lives under -cert-dir, and signs one name: whatever -balancer-name says.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	caCrtFile     = "ca.crt"
	caKeyFile     = "ca.key"
	serverCrtFile = "server.crt"
	serverKeyFile = "server.key"
	lockFile      = ".lock"

	// Long enough that a bench camera's trust bundle does not quietly expire
	// mid-project, and backdated an hour because a camera whose clock NTP has
	// not corrected yet is usually behind — and "not valid yet" fails in a way
	// that reads exactly like a broken CA.
	validFor = 10 * 365 * 24 * time.Hour
	backdate = time.Hour
)

// Ensure returns a server certificate for name, issued by a CA under dir,
// creating either where it is missing or no longer usable. The CA certificate
// is left at dir/ca.crt, which is what goes into the camera's trust store.
func Ensure(dir, name string) (tls.Certificate, error) {
	if name == "" {
		return tls.Certificate{}, errors.New("certificates: no name to issue for")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, err
	}

	/* One writer at a time. Two instances started together — different ports,
	   same default -cert-dir — would otherwise each mint a root, and the one
	   that lost the race would serve a leaf chaining to a CA that is no longer
	   the ca.crt on disk. Cameras trust the file, so that instance becomes
	   unreachable and the run reports "the camera never connected": a FAIL
	   that is not the camera's fault, which is the one thing this tool must
	   not produce. */
	unlock, err := lock(dir)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer unlock()

	caCert, caKey, err := ensureCA(dir)
	if err != nil {
		return tls.Certificate{}, err
	}

	// The CA comes first so reuse can be checked against it. A server
	// certificate that is still in date but was signed by a CA since replaced
	// would chain to nothing the camera has, which looks like a camera problem
	// rather than a stale file.
	if pair, ok := reusable(dir, name, caCert); ok {
		return pair, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl, err := template(pkix.Name{CommonName: name})
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl.DNSNames = []string{name}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := write(dir, serverCrtFile, serverKeyFile, der, key); err != nil {
		return tls.Certificate{}, err
	}

	// The CA rides along, so a camera that trusts only the root can still
	// build a path.
	return tls.Certificate{
		Certificate: [][]byte{der, caCert.Raw},
		PrivateKey:  key,
	}, nil
}

// reusable answers whether the server certificate already on disk can be
// served as-is: issued by this CA, for this name, and still in date.
func reusable(dir, name string, ca *x509.Certificate) (tls.Certificate, bool) {
	pair, err := tls.LoadX509KeyPair(file(dir, serverCrtFile), file(dir, serverKeyFile))
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	if leaf.VerifyHostname(name) != nil {
		return tls.Certificate{}, false
	}
	if !within(leaf) {
		return tls.Certificate{}, false
	}
	if leaf.CheckSignatureFrom(ca) != nil {
		return tls.Certificate{}, false
	}
	pair.Certificate = [][]byte{pair.Certificate[0], ca.Raw}
	return pair, true
}

func ensureCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if cert, key, err := loadCA(dir); err == nil {
		return cert, key, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl, err := template(pkix.Name{
		Organization: []string{"ipeye-tt"},
		CommonName:   "ipeye-tt test CA",
	})
	if err != nil {
		return nil, nil, err
	}
	tmpl.IsCA = true
	tmpl.BasicConstraintsValid = true
	tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := write(dir, caCrtFile, caKeyFile, der, key); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	crtPEM, err := os.ReadFile(file(dir, caCrtFile))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(file(dir, caKeyFile))
	if err != nil {
		return nil, nil, err
	}
	crtBlock, _ := pem.Decode(crtPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if crtBlock == nil || keyBlock == nil {
		return nil, nil, errors.New("certificates: the CA on disk is not PEM")
	}
	cert, err := x509.ParseCertificate(crtBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}

	/* Parsing is not usability. Anything wrong here produces a chain that
	   clients trusting ca.crt reject, and a rejected handshake reads as a
	   camera fault — so each of these is a reason to mint a fresh authority
	   rather than to carry on with a broken one. */
	if !within(cert) {
		return nil, nil, errors.New("certificates: the CA on disk is not currently valid")
	}
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("certificates: the certificate on disk cannot sign")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, errors.New("certificates: the CA key does not match its certificate")
	}
	return cert, key, nil
}

/*
Valid now, at both ends. The upper bound is the obvious one; the lower

	matters because certificates are issued relative to this host's clock, and a
	clock that was ahead when one was written and has since been corrected
	leaves it not valid *yet* — which fails handshakes just as completely.
*/
func within(cert *x509.Certificate) bool {
	now := time.Now()
	return !now.Before(cert.NotBefore) && !now.After(cert.NotAfter)
}

/*
An exclusive lock on the certificate directory, released by the returned

	function. flock, so it is held by the open file description and released
	even if the process dies.
*/
func lock(dir string) (func(), error) {
	f, err := os.OpenFile(file(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func template(subject pkix.Name) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      subject,
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(validFor),
	}, nil
}

func write(dir, crtName, keyName string, der []byte, key *ecdsa.PrivateKey) error {
	if err := replace(file(dir, crtName),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return replace(file(dir, keyName),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

/*
Write through a temporary file and rename over the target.

	Not for atomicity alone: os.WriteFile's mode applies only when it creates
	the file, so rewriting a key that already existed with permissive bits left
	the private material readable by every local user while reporting success.
	A fresh inode carries the mode asked for, whatever was there before.
*/
func replace(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func file(dir, name string) string { return filepath.Join(dir, name) }
