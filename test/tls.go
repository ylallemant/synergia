package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ensureTLSCerts generates a self-signed CA and localhost certificate if they
// don't already exist in dir. Returns paths to ca.crt, server.crt, server.key.
func ensureTLSCerts(dir string) (caCert, serverCert, serverKey string, err error) {
	caCert = filepath.Join(dir, "ca.crt")
	serverCert = filepath.Join(dir, "server.crt")
	serverKey = filepath.Join(dir, "server.key")

	// If all files exist, reuse them
	if fileExists(caCert) && fileExists(serverCert) && fileExists(serverKey) {
		return caCert, serverCert, serverKey, nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", "", fmt.Errorf("create TLS dir: %w", err)
	}

	// Generate CA key
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate CA key: %w", err)
	}

	// CA certificate template
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Synergia Test CA"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", fmt.Errorf("create CA cert: %w", err)
	}

	// Write CA cert
	if err := writePEM(caCert, "CERTIFICATE", caDER); err != nil {
		return "", "", "", err
	}

	// Generate server key
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", fmt.Errorf("generate server key: %w", err)
	}

	// Server certificate template
	srvTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"Synergia Test"},
			CommonName:   "localhost",
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}

	// Sign server cert with CA
	caParsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", "", "", fmt.Errorf("parse CA cert: %w", err)
	}

	srvDER, err := x509.CreateCertificate(rand.Reader, srvTemplate, caParsed, &srvKey.PublicKey, caKey)
	if err != nil {
		return "", "", "", fmt.Errorf("create server cert: %w", err)
	}

	// Write server cert
	if err := writePEM(serverCert, "CERTIFICATE", srvDER); err != nil {
		return "", "", "", err
	}

	// Write server key
	keyBytes, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal server key: %w", err)
	}
	if err := writePEM(serverKey, "EC PRIVATE KEY", keyBytes); err != nil {
		return "", "", "", err
	}

	return caCert, serverCert, serverKey, nil
}

func writePEM(path, blockType string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}
