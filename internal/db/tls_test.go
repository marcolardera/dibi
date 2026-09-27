package db

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dibi/internal/config"
)

// testCA returns a CA and a server certificate it signed for db.test.
func testCA(t *testing.T) (caPEM []byte, server tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "db.test"}, DNSNames: []string{"db.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// handshake runs a TLS handshake with a server presenting cert.
func handshake(t *testing.T, cfg *tls.Config, cert tls.Certificate) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if conn, err := ln.Accept(); err == nil {
			conn.(*tls.Conn).Handshake()
			conn.Close()
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), cfg)
	if err == nil {
		conn.Close()
	}
	return err
}

func TestTLSConfig(t *testing.T) {
	caPEM, cert := testCA(t)
	otherPEM, _ := testCA(t)
	dir := t.TempDir()
	ca, other := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "other.pem")
	os.WriteFile(ca, caPEM, 0o600)
	os.WriteFile(other, otherPEM, 0o600)

	tests := []struct {
		mode   config.SSLMode
		root   string
		host   string
		wantOK bool
	}{
		{config.SSLPrefer, "", "db.test", true},
		{config.SSLRequire, "", "wrong.test", true},
		{config.SSLRequire, other, "db.test", true}, // the CA is ignored without verification
		{config.SSLVerifyCA, ca, "wrong.test", true},
		{config.SSLVerifyCA, other, "db.test", false},
		{config.SSLVerifyCA, "", "db.test", false}, // not signed by a system CA
		{config.SSLVerifyFull, ca, "db.test", true},
		{config.SSLVerifyFull, ca, "wrong.test", false},
		{config.SSLVerifyFull, other, "db.test", false},
	}
	for _, tt := range tests {
		c := config.Connection{Type: config.Postgres, SSLMode: tt.mode, SSLRootCert: tt.root}
		cfg, err := tlsConfig(c, tt.host)
		if err != nil {
			t.Fatal(err)
		}
		err = handshake(t, cfg, cert)
		if (err == nil) != tt.wantOK {
			t.Errorf("%s root=%q host=%s: err=%v, want ok=%v", tt.mode, filepath.Base(tt.root), tt.host, err, tt.wantOK)
		}
	}

	if cfg, _ := tlsConfig(config.Connection{Type: config.Postgres, SSLMode: config.SSLDisable}, "db.test"); cfg != nil {
		t.Error("disable must not use TLS")
	}
	if cfg, _ := tlsConfig(config.Connection{Type: config.Oracle}, "db.test"); cfg != nil {
		t.Error("Oracle must not use TLS by default")
	}
	if _, err := tlsConfig(config.Connection{Type: config.Postgres, SSLMode: config.SSLVerifyFull, SSLRootCert: filepath.Join(dir, "missing.pem")}, "db.test"); err == nil {
		t.Error("expected an error for a missing CA file")
	}
}
