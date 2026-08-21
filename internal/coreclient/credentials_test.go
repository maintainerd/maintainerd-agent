package coreclient

import (
	"context"
	"errors"
	"testing"

	sdk "github.com/maintainerd/sdk"
)

type failingCreds struct{}

func (failingCreds) Token(context.Context) (string, error) {
	return "", errors.New("token endpoint down")
}

func TestPerRPCCredentialsAttachesBearer(t *testing.T) {
	p := perRPCCredentials{creds: sdk.StaticToken("tok-123")}
	md, err := p.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if md["authorization"] != "Bearer tok-123" {
		t.Errorf("authorization = %q, want Bearer tok-123", md["authorization"])
	}
}

func TestPerRPCCredentialsPropagatesTokenError(t *testing.T) {
	p := perRPCCredentials{creds: failingCreds{}}
	if _, err := p.GetRequestMetadata(context.Background()); err == nil {
		t.Fatal("token-source failure must fail the call, not send it anonymously")
	}
}

func TestPerRPCCredentialsAnonymousSendsNothing(t *testing.T) {
	p := perRPCCredentials{creds: sdk.Anonymous{}}
	md, err := p.GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetRequestMetadata: %v", err)
	}
	if len(md) != 0 {
		t.Errorf("metadata = %v, want empty for anonymous", md)
	}
}
