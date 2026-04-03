package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureTLSCertAt_generatesFreshCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	gotCert, gotKey, err := ensureTLSCertAt(certPath, keyPath)
	if err != nil {
		t.Fatalf("ensureTLSCertAt: %v", err)
	}
	if gotCert != certPath {
		t.Errorf("certFile: got %q, want %q", gotCert, certPath)
	}
	if gotKey != keyPath {
		t.Errorf("keyFile: got %q, want %q", gotKey, keyPath)
	}

	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file not written: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file not written: %v", err)
	}
}

func TestEnsureTLSCertAt_reusesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	// Generate once.
	if _, _, err := ensureTLSCertAt(certPath, keyPath); err != nil {
		t.Fatalf("first call: %v", err)
	}
	certStat1, _ := os.Stat(certPath)

	// Call again — must not regenerate (mtime unchanged).
	if _, _, err := ensureTLSCertAt(certPath, keyPath); err != nil {
		t.Fatalf("second call: %v", err)
	}
	certStat2, _ := os.Stat(certPath)

	if certStat1.ModTime() != certStat2.ModTime() {
		t.Error("cert was regenerated on second call; expected reuse")
	}
}

func TestEnsureTLSCertAt_certIsValidForLocalhost(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if _, _, err := ensureTLSCertAt(certPath, keyPath); err != nil {
		t.Fatalf("ensureTLSCertAt: %v", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("cert file is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	// Must include localhost DNS SAN.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	_, err = cert.Verify(x509.VerifyOptions{
		DNSName: "localhost",
		Roots:   pool,
	})
	if err != nil {
		t.Errorf("cert not valid for localhost: %v", err)
	}

	// Must include 127.0.0.1 IP SAN.
	found127 := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(net.ParseIP("127.0.0.1")) {
			found127 = true
		}
	}
	if !found127 {
		t.Error("cert missing 127.0.0.1 IP SAN")
	}
}

func TestEnsureTLSCertAt_certAndKeyMatchForTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if _, _, err := ensureTLSCertAt(certPath, keyPath); err != nil {
		t.Fatalf("ensureTLSCertAt: %v", err)
	}

	// tls.LoadX509KeyPair verifies the cert and key are a matching pair.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("cert/key pair invalid: %v", err)
	}
}

func TestEnsureTLSCertAt_certHasTenYearValidity(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	if _, _, err := ensureTLSCertAt(certPath, keyPath); err != nil {
		t.Fatalf("ensureTLSCertAt: %v", err)
	}

	certPEM, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	years := cert.NotAfter.Sub(cert.NotBefore).Hours() / (24 * 365)
	if years < 9.9 {
		t.Errorf("expected ~10 year validity, got %.1f years", years)
	}
}
