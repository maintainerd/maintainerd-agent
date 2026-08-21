package coreclient

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	sdk "github.com/maintainerd/sdk"
)

// perRPCCredentials adapts an sdk token source (PrivateKeyJWT or
// ClientCredentials) to gRPC per-RPC credentials, so EVERY call the agent
// makes to Core carries its identity token. The agent is an auth principal
// like any other service: Core's gateway verifies the token and enforces
// permissions — an unauthenticated agent channel would let anyone who can
// reach Core's port impersonate a fleet host.
type perRPCCredentials struct {
	creds sdk.Credentials
}

var _ credentials.PerRPCCredentials = perRPCCredentials{}

// GetRequestMetadata mints/returns the cached token and attaches it as the
// authorization header. Errors propagate — a call without identity should
// fail here, loudly, not reach Core anonymously.
func (p perRPCCredentials) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	tok, err := p.creds.Token(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, nil
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

// RequireTransportSecurity returns false so development plaintext channels
// work; production transport security is decided by the dial options (TLS
// creds), not silently forced here. The fail-closed decision — refusing to
// start without token credentials outside development — lives in the
// bootstrap, where it can refuse BEFORE any connection exists.
func (p perRPCCredentials) RequireTransportSecurity() bool { return false }

// WithCredentials returns the dial option that attaches the agent's identity
// to every RPC on the connection.
func WithCredentials(creds sdk.Credentials) grpc.DialOption {
	return grpc.WithPerRPCCredentials(perRPCCredentials{creds: creds})
}
