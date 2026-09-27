package db

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dibi/internal/config"

	"github.com/sijms/go-ora/v2/configurations"
)

// tlsConfig returns the TLS settings of a server connection, or nil when its
// SSL mode is disable. host is the name checked by verify-full and sent for
// SNI. Prefer and require encrypt without checking the server certificate;
// verify-ca checks that it was signed by a trusted CA (SSLRootCert and the
// wallet, or the system CAs); verify-full also checks that it names host.
// Whether prefer falls back to plaintext is up to the caller.
func tlsConfig(c config.Connection, host string) (*tls.Config, error) {
	mode := c.TLSMode()
	if mode == config.SSLDisable {
		return nil, nil
	}
	cfg := &tls.Config{ServerName: host}
	var roots *x509.CertPool
	if mode.Verifies() && c.SSLRootCert != "" {
		pem, err := os.ReadFile(c.SSLRootCert)
		if err != nil {
			return nil, fmt.Errorf("CA certificate: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA certificate: no PEM certificate in %s", c.SSLRootCert)
		}
	}
	if c.SSLCert != "" {
		cert, err := tls.LoadX509KeyPair(c.SSLCert, c.SSLKey)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.Certificates = append(cfg.Certificates, cert)
	}
	if c.Type == config.Oracle && c.Wallet != "" {
		var err error
		if roots, err = loadWallet(c.Wallet, cfg, roots, mode.Verifies()); err != nil {
			return nil, err
		}
	}

	switch mode {
	case config.SSLVerifyFull:
		cfg.RootCAs = roots
	case config.SSLVerifyCA:
		// Go checks the host name together with the chain, so the chain is
		// verified by hand.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server sent no certificate")
			}
			opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
			for _, cert := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		}
	default:
		cfg.InsecureSkipVerify = true
	}
	return cfg, nil
}

// loadWallet reads the auto-login wallet (cwallet.sso) of an Oracle wallet
// folder, as downloaded for an Autonomous Database. Its key pair becomes the
// client certificate and, when verifying, its certificates are added to the
// trusted CAs, which it returns.
func loadWallet(dir string, cfg *tls.Config, roots *x509.CertPool, verify bool) (*x509.CertPool, error) {
	w, err := configurations.NewWallet(filepath.Join(dir, "cwallet.sso"))
	if err != nil {
		return nil, fmt.Errorf("wallet: %w", err)
	}
	var certs []*x509.Certificate
	for _, der := range w.Certificates {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("wallet: %w", err)
		}
		certs = append(certs, cert)
	}
	for _, der := range w.PrivateKeys {
		key, err := x509.ParsePKCS1PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("wallet: %w", err)
		}
		for _, cert := range certs {
			if key.PublicKey.Equal(cert.PublicKey) {
				cfg.Certificates = append(cfg.Certificates,
					tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert})
			}
		}
	}
	if verify && len(certs) > 0 {
		if roots == nil {
			roots = x509.NewCertPool()
		}
		for _, cert := range certs {
			roots.AddCert(cert)
		}
	}
	return roots, nil
}
