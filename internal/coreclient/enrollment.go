package coreclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type IdentityFiles struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// IdentityReady reports whether the local enrolled client certificate/key pair
// is complete. A half-written identity is a hard error: using or overwriting it
// silently could strand the one-time join token.
func IdentityReady(files IdentityFiles) (bool, error) {
	certOK, err := fileExists(files.CertFile)
	if err != nil {
		return false, err
	}
	keyOK, err := fileExists(files.KeyFile)
	if err != nil {
		return false, err
	}
	if certOK != keyOK {
		return false, fmt.Errorf("partial agent identity: both %s and %s must exist or neither must exist", files.CertFile, files.KeyFile)
	}
	return certOK && keyOK, nil
}

func EnsureEnrolled(ctx context.Context, addr, agentUUID, joinToken string, files IdentityFiles, transport credentials.TransportCredentials) (bool, error) {
	ready, err := IdentityReady(files)
	if err != nil || ready {
		return false, err
	}
	if agentUUID == "" {
		return false, fmt.Errorf("AGENT_UUID is required for Core enrollment")
	}
	if joinToken == "" {
		return false, nil
	}
	if files.CertFile == "" || files.KeyFile == "" {
		return false, fmt.Errorf("agent client certificate and key file paths are required")
	}
	keyPEM, csrPEM, err := newEnrollmentCSR(agentUUID)
	if err != nil {
		return false, err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(transport))
	if err != nil {
		return false, fmt.Errorf("dial core for enrollment: %w", err)
	}
	defer func() { _ = conn.Close() }()
	enrolled, err := New(conn).Enroll(ctx, agentUUID, joinToken, csrPEM)
	if err != nil {
		return false, fmt.Errorf("enroll agent with core: %w", err)
	}
	if err := writeIdentity(files, keyPEM, enrolled); err != nil {
		return false, err
	}
	return true, nil
}

func newEnrollmentCSR(agentUUID string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate agent key: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal agent key: %w", err)
	}
	agentURI, err := url.Parse("spiffe://maintainerd/agent/" + agentUUID)
	if err != nil {
		return nil, nil, fmt.Errorf("build agent URI SAN: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "maintainerd-agent:" + agentUUID},
		URIs:    []*url.URL{agentURI},
	}, key)
	if err != nil {
		return nil, nil, fmt.Errorf("build agent CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}),
		nil
}

func writeIdentity(files IdentityFiles, keyPEM []byte, enrolled *Enrollment) error {
	if enrolled == nil || len(enrolled.CertificatePEM) == 0 {
		return fmt.Errorf("core enrollment returned no client certificate")
	}
	if err := writeFileAtomic(files.KeyFile, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write agent client key: %w", err)
	}
	if err := writeFileAtomic(files.CertFile, enrolled.CertificatePEM, 0o600); err != nil {
		return fmt.Errorf("write agent client certificate: %w", err)
	}
	if files.CAFile != "" && len(enrolled.CACertificatePEM) > 0 {
		if err := writeFileAtomic(files.CAFile, enrolled.CACertificatePEM, 0o644); err != nil {
			return fmt.Errorf("write agent client CA certificate: %w", err)
		}
	}
	return nil
}

func fileExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
