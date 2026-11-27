package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The devnet's TLS material for the signer, in one folder: a CA of its own, the
// signer's server certificate and the payer's client certificate. In production these
// come from the organisation's PKI; the signer and the payer read the same files.
const (
	CAFile         = "ca.pem"
	caKeyFile      = "ca-key.pem"
	ServerCertFile = "signer.pem"
	ServerKeyFile  = "signer-key.pem"
	ClientCertFile = "payer.pem"
	ClientKeyFile  = "payer-key.pem"
	// ServerName is the signer's host name inside Docker, in its certificate.
	ServerName = "withdrawal-signer"
	// ClientName is the payer's common name, the one the signer allows.
	ClientName = "withdrawal-payer"
)

// validity is how long a certificate lasts; renewBefore how long before its end it is
// made again.
const (
	validity    = 365 * 24 * time.Hour
	renewBefore = 30 * 24 * time.Hour
)

// EnsureTLS makes the CA and both certificates in dir when one is missing, does not
// chain to the CA, or ends within 30 days; otherwise it changes nothing. dir must be
// readable by its owner only. The CA's key is 600; the others are 644, read by the
// services' own users inside their containers. It reports whether it wrote.
func EnsureTLS(dir string, now time.Time) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("signer: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("signer: %s has mode %o, want 700", dir, info.Mode().Perm())
	}
	if valid(dir, now) == nil {
		return false, nil
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "settled devnet signer CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(validity),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return false, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return false, err
	}
	leaf := func(name string, usage x509.ExtKeyUsage, dns []string, ips []net.IP) ([]byte, *ecdsa.PrivateKey, error) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(validity),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
			DNSNames: dns, IPAddresses: ips,
		}, ca, &key.PublicKey, caKey)
		return der, key, err
	}
	serverDER, serverKey, err := leaf(ServerName, x509.ExtKeyUsageServerAuth, []string{ServerName, "localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return false, err
	}
	clientDER, clientKey, err := leaf(ClientName, x509.ExtKeyUsageClientAuth, nil, nil)
	if err != nil {
		return false, err
	}
	for _, f := range []struct {
		name  string
		block string
		der   []byte
		key   *ecdsa.PrivateKey
		mode  os.FileMode
	}{
		{CAFile, "CERTIFICATE", caDER, nil, 0o644},
		{caKeyFile, "", nil, caKey, 0o600},
		{ServerCertFile, "CERTIFICATE", serverDER, nil, 0o644},
		{ServerKeyFile, "", nil, serverKey, 0o644},
		{ClientCertFile, "CERTIFICATE", clientDER, nil, 0o644},
		{ClientKeyFile, "", nil, clientKey, 0o644},
	} {
		der, block := f.der, f.block
		if f.key != nil {
			if der, err = x509.MarshalECPrivateKey(f.key); err != nil {
				return false, err
			}
			block = "EC PRIVATE KEY"
		}
		if err := writeFile(filepath.Join(dir, f.name), pem.EncodeToMemory(&pem.Block{Type: block, Bytes: der}), f.mode); err != nil {
			return false, err
		}
	}
	return true, nil
}

// valid checks that every file is there, chains to the CA, and lasts more than 30 days.
func valid(dir string, now time.Time) error {
	read := func(name string) (*x509.Certificate, error) {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%s holds no certificate", name)
		}
		return x509.ParseCertificate(block.Bytes)
	}
	ca, err := read(CAFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, c := range []struct {
		cert, key string
		usage     x509.ExtKeyUsage
	}{{ServerCertFile, ServerKeyFile, x509.ExtKeyUsageServerAuth}, {ClientCertFile, ClientKeyFile, x509.ExtKeyUsageClientAuth}} {
		cert, err := read(c.cert)
		if err != nil {
			return err
		}
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now.Add(renewBefore), KeyUsages: []x509.ExtKeyUsage{c.usage}}); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, c.key)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(dir, caKeyFile)); err != nil {
		return errors.Join(err, errors.New("the CA's key is missing"))
	}
	return nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err)
	}
	return n
}

// writeFile writes through a temporary file and a rename, with mode set exactly.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
