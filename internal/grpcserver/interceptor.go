package grpcserver

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Claims is the verified identity of a caller, as the interceptor needs it:
// the scopes from the token's "scope" claim plus any "permissions" array
// claim. Both are checked because maintainerd-auth can mint either shape.
type Claims struct {
	Subject     string
	Scopes      []string
	Permissions []string
}

// VerifyFunc validates a bearer token and returns its claims. In production
// it wraps the sdk verifier (JWKS + issuer + audience checks); tests inject
// their own.
type VerifyFunc func(ctx context.Context, token string) (*Claims, error)

// methodPermissions maps every AgentService method to the permission its
// caller must carry — the same method→permission enforcement style as
// maintainerd-auth's gRPC surface. An empty value means "any authenticated
// principal" (identity proven, no extra grant needed).
//
// The map doubles as the surface allowlist: a method that is NOT listed here
// is DENIED even to authenticated callers, so adding an RPC to the proto
// without deciding its permission fails closed instead of shipping an
// unguarded endpoint.
var methodPermissions = map[string]string{
	"/maintainerd.agent.v1.AgentService/Ping": "",                // authenticated liveness probe
	"/maintainerd.agent.v1.AgentService/Info": "agent:info:read", // host/runtime details are recon material
}

// healthServicePrefix is the ONLY unauthenticated gRPC surface: the standard
// health protocol. Orchestrators and load balancers must be able to probe
// liveness before they have credentials, and the health response leaks
// nothing beyond "serving".
const healthServicePrefix = "/grpc.health.v1.Health/"

// AuthUnaryInterceptor enforces bearer-token authentication and per-method
// permissions on every unary RPC. Fail-closed by construction:
//
//   - no token            → Unauthenticated
//   - invalid token       → Unauthenticated
//   - method not mapped   → PermissionDenied (unknown surface, deny)
//   - permission missing  → PermissionDenied
//
// Only grpc.health.v1.Health is exempt (see healthServicePrefix).
func AuthUnaryInterceptor(verify VerifyFunc) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, healthServicePrefix) {
			return handler(ctx, req)
		}
		token := bearerFromMD(ctx)
		if token == "" {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		claims, err := verify(ctx, token)
		if err != nil {
			// The verify error is deliberately not echoed to the caller —
			// "why exactly your forged token failed" is oracle material.
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		required, known := methodPermissions[info.FullMethod]
		if !known {
			return nil, status.Errorf(codes.PermissionDenied, "method %s has no permission mapping", info.FullMethod)
		}
		if required != "" && !claims.hasPermission(required) {
			return nil, status.Errorf(codes.PermissionDenied, "requires permission %s", required)
		}
		return handler(ctx, req)
	}
}

// hasPermission checks membership in either claim shape. An absent claim is
// simply "no permissions" — never a bypass.
func (c *Claims) hasPermission(perm string) bool {
	if c == nil {
		return false
	}
	for _, s := range c.Scopes {
		if s == perm {
			return true
		}
	}
	for _, p := range c.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// bearerFromMD extracts a "Bearer <token>" authorization header from the
// incoming gRPC metadata.
func bearerFromMD(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return ""
	}
	header := vals[0]
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}
