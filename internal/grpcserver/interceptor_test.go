package grpcserver

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fixed test identities keyed by token.
var testIdentities = map[string]*Claims{
	"tok-reader":      {Subject: "svc-console", Scopes: []string{"agent:info:read"}},
	"tok-perms-claim": {Subject: "svc-array", Permissions: []string{"agent:info:read"}},
	"tok-bare":        {Subject: "svc-bare"}, // authenticated, no permissions at all
}

func testVerify(_ context.Context, token string) (*Claims, error) {
	if c, ok := testIdentities[token]; ok {
		return c, nil
	}
	return nil, errors.New("bad token")
}

func ctxWithToken(token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

func invoke(t *testing.T, ctx context.Context, fullMethod string) error {
	t.Helper()
	interceptor := AuthUnaryInterceptor(testVerify)
	handler := func(context.Context, any) (any, error) { return "ok", nil }
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod}, handler)
	return err
}

func TestAuthUnaryInterceptor(t *testing.T) {
	const (
		ping = "/maintainerd.agent.v1.AgentService/Ping"
		info = "/maintainerd.agent.v1.AgentService/Info"
	)
	tests := []struct {
		name     string
		ctx      context.Context
		method   string
		wantCode codes.Code
	}{
		{
			name:     "health is the only unauthenticated surface",
			ctx:      context.Background(),
			method:   "/grpc.health.v1.Health/Check",
			wantCode: codes.OK,
		},
		{
			name:     "missing token is Unauthenticated",
			ctx:      context.Background(),
			method:   ping,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "invalid token is Unauthenticated",
			ctx:      ctxWithToken("forged"),
			method:   ping,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "malformed authorization header is Unauthenticated",
			ctx:      metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Basic dXNlcg==")),
			method:   ping,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "Ping needs authentication only",
			ctx:      ctxWithToken("tok-bare"),
			method:   ping,
			wantCode: codes.OK,
		},
		{
			name:     "Info without the permission is PermissionDenied",
			ctx:      ctxWithToken("tok-bare"),
			method:   info,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "Info with scope claim permission passes",
			ctx:      ctxWithToken("tok-reader"),
			method:   info,
			wantCode: codes.OK,
		},
		{
			name:     "Info with permissions array claim passes",
			ctx:      ctxWithToken("tok-perms-claim"),
			method:   info,
			wantCode: codes.OK,
		},
		{
			name:     "unmapped method fails closed even when authenticated",
			ctx:      ctxWithToken("tok-reader"),
			method:   "/maintainerd.agent.v1.AgentService/FutureUnmappedRPC",
			wantCode: codes.PermissionDenied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := invoke(t, tt.ctx, tt.method)
			if got := status.Code(err); got != tt.wantCode {
				t.Errorf("code = %v (err %v), want %v", got, err, tt.wantCode)
			}
		})
	}
}

func TestClaimsHasPermission(t *testing.T) {
	tests := []struct {
		name   string
		claims *Claims
		want   bool
	}{
		{"nil claims denied", nil, false},
		{"empty claims denied — absent claim is never a bypass", &Claims{}, false},
		{"scope membership", &Claims{Scopes: []string{"a", "agent:info:read"}}, true},
		{"permissions membership", &Claims{Permissions: []string{"agent:info:read"}}, true},
		{"near-miss denied", &Claims{Scopes: []string{"agent:info"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.claims.hasPermission("agent:info:read"); got != tt.want {
				t.Errorf("hasPermission = %v, want %v", got, tt.want)
			}
		})
	}
}
