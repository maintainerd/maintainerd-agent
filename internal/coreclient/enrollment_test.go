package coreclient

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityReadyRequiresCompletePair(t *testing.T) {
	dir := t.TempDir()
	files := IdentityFiles{
		CertFile: filepath.Join(dir, "agent.crt"),
		KeyFile:  filepath.Join(dir, "agent.key"),
	}

	ready, err := IdentityReady(files)
	if err != nil {
		t.Fatalf("IdentityReady empty: %v", err)
	}
	if ready {
		t.Fatal("empty identity should not be ready")
	}

	if err := os.WriteFile(files.CertFile, []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := IdentityReady(files); err == nil {
		t.Fatal("partial identity should fail")
	}

	if err := os.WriteFile(files.KeyFile, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready, err = IdentityReady(files)
	if err != nil {
		t.Fatalf("IdentityReady complete: %v", err)
	}
	if !ready {
		t.Fatal("complete identity should be ready")
	}
}

func TestNewEnrollmentCSRIncludesAgentIdentity(t *testing.T) {
	_, csrPEM, err := newEnrollmentCSR("agent-123")
	if err != nil {
		t.Fatalf("newEnrollmentCSR: %v", err)
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		t.Fatal("CSR PEM did not decode")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature: %v", err)
	}
	if csr.Subject.CommonName != "maintainerd-agent:agent-123" {
		t.Fatalf("CommonName = %q", csr.Subject.CommonName)
	}
	if len(csr.URIs) != 1 || csr.URIs[0].String() != "spiffe://maintainerd/agent/agent-123" {
		t.Fatalf("URIs = %v", csr.URIs)
	}
}
