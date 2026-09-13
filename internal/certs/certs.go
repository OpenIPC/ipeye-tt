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
	"time"
)

const (
	caCrtFile     = "ca.crt"
	caKeyFile     = "ca.key"
	serverCrtFile = "server.crt"
	serverKeyFile = "server.key"

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
	if time.Now().After(leaf.NotAfter) {
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
	if time.Now().After(cert.NotAfter) {
		return nil, nil, errors.New("certificates: the CA on disk has expired")
	}
	return cert, key, nil
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
	if err := os.WriteFile(file(dir, crtName),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(file(dir, keyName),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func file(dir, name string) string { return filepath.Join(dir, name) }
