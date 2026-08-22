package coreclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type TransportOptions struct {
	CAFile     string
	ServerName string
	CertFile   string
	KeyFile    string
	AllowPlain bool
}

// TransportCredentials builds the Core dial transport. TLS is selected when
// any TLS material is configured; plaintext is allowed only when the bootstrap
// marks the process as development.
func TransportCredentials(opts TransportOptions) (credentials.TransportCredentials, bool, error) {
	hasTLS := opts.CAFile != "" || opts.ServerName != "" || opts.CertFile != "" || opts.KeyFile != ""
	if !hasTLS {
		if !opts.AllowPlain {
			return nil, false, fmt.Errorf("Core gRPC TLS is required outside development")
		}
		return insecure.NewCredentials(), false, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: opts.ServerName}
	if opts.CAFile != "" {
		pool, err := certPoolFromFile(opts.CAFile)
		if err != nil {
			return nil, false, err
		}
		cfg.RootCAs = pool
	}
	if opts.CertFile != "" || opts.KeyFile != "" {
		if opts.CertFile == "" || opts.KeyFile == "" {
			return nil, false, fmt.Errorf("AGENT_CLIENT_CERT_FILE and AGENT_CLIENT_KEY_FILE must be configured together")
		}
		cert, err := tls.LoadX509KeyPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, false, fmt.Errorf("load agent client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(cfg), true, nil
}

func certPoolFromFile(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CORE_TLS_CA_FILE: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("CORE_TLS_CA_FILE contains no PEM certificates")
	}
	return pool, nil
}
