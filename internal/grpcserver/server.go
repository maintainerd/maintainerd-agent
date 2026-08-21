// Package grpcserver serves the agent.v1.AgentService contract this repo owns:
// the agent's local control/health surface. The agent is outbound-only by
// design — this is its ONE inbound listener, so it is guarded the way
// maintainerd-auth guards its own surfaces: sdk-verified bearer tokens plus a
// per-method permission map, failing closed when auth is not configured.
package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	sdkauth "github.com/maintainerd/sdk/auth"

	kitruntime "github.com/maintainerd/kit/runtime"

	agentv1 "github.com/maintainerd/agent/gen/maintainerd/agent/v1"
)

// GuardMode is how the listener treats callers.
type GuardMode int

const (
	// GuardEnforced verifies tokens and permissions on every RPC — the only
	// mode outside development.
	GuardEnforced GuardMode = iota
	// GuardDevOpen serves without authentication. Permitted ONLY in
	// development, and announced loudly at boot so an open surface can never
	// be a quiet surprise.
	GuardDevOpen
	// GuardHealthOnly refuses to serve AgentService at all: only the standard
	// health protocol is registered. This is the fail-closed posture when
	// auth is required but not configured — a control surface with no guard
	// simply does not come up.
	GuardHealthOnly
)

// Guard is the resolved inbound-auth posture, decided at startup by the
// bootstrap (see cmd/agentd).
type Guard struct {
	Mode   GuardMode
	Verify VerifyFunc // required when Mode == GuardEnforced
	Reason string     // human-readable cause for DevOpen/HealthOnly logging
	Dev    bool       // development environment: enables gRPC reflection
}

// SDKVerify adapts the sdk verifier to the interceptor's VerifyFunc, mapping
// both permission claim shapes: the space-separated "scope" claim (parsed by
// the sdk) and a "permissions" array claim, either of which maintainerd-auth
// may mint.
func SDKVerify(v *sdkauth.Verifier) VerifyFunc {
	return func(_ context.Context, token string) (*Claims, error) {
		c, err := v.Verify(token)
		if err != nil {
			return nil, err
		}
		out := &Claims{Subject: c.Subject, Scopes: c.Scopes}
		if raw, ok := c.Raw["permissions"].([]any); ok {
			for _, p := range raw {
				if s, ok := p.(string); ok {
					out.Permissions = append(out.Permissions, s)
				}
			}
		}
		return out, nil
	}
}

// Service implements agentv1.AgentServiceServer.
type Service struct {
	agentv1.UnimplementedAgentServiceServer
	name    string
	version string
	rt      kitruntime.Runtime
}

func NewService(name, version string, rt kitruntime.Runtime) *Service {
	return &Service{name: name, version: version, rt: rt}
}

func (s *Service) Ping(_ context.Context, _ *agentv1.PingRequest) (*agentv1.PingResponse, error) {
	return &agentv1.PingResponse{Ok: true}, nil
}

func (s *Service) Info(ctx context.Context, _ *agentv1.InfoRequest) (*agentv1.InfoResponse, error) {
	runtimeConnected := s.rt.Ping(ctx) == nil
	return &agentv1.InfoResponse{
		Name:             s.name,
		Version:          s.version,
		Status:           "ready",
		RuntimeConnected: runtimeConnected,
	}, nil
}

// Serve starts the gRPC server per the guard's posture and stops it
// gracefully when ctx is cancelled.
func Serve(ctx context.Context, addr string, svc *Service, guard Guard) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	var opts []grpc.ServerOption
	switch guard.Mode {
	case GuardEnforced:
		opts = append(opts, grpc.ChainUnaryInterceptor(AuthUnaryInterceptor(guard.Verify)))
	case GuardDevOpen:
		slog.Warn("SECURITY: gRPC surface is UNAUTHENTICATED (development only)",
			"disabled_guards", "bearer-token verification, per-method permissions (agent:info:read)",
			"reason", guard.Reason,
		)
	case GuardHealthOnly:
		slog.Error("gRPC AgentService REFUSING to start — serving health only", "reason", guard.Reason)
	}

	gs := grpc.NewServer(opts...)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)

	if guard.Mode == GuardHealthOnly {
		hs.SetServingStatus("maintainerd.agent.v1.AgentService", healthpb.HealthCheckResponse_NOT_SERVING)
	} else {
		agentv1.RegisterAgentServiceServer(gs, svc)
		hs.SetServingStatus("maintainerd.agent.v1.AgentService", healthpb.HealthCheckResponse_SERVING)
	}

	// Reflection is a discovery aid and an attacker's site map; development only.
	if guard.Dev {
		reflection.Register(gs)
	}

	go func() {
		<-ctx.Done()
		slog.Info("shutting down grpc server")
		gs.GracefulStop()
	}()

	slog.Info("grpc server listening", "addr", addr, "guard", guardModeName(guard.Mode))
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

func guardModeName(m GuardMode) string {
	switch m {
	case GuardEnforced:
		return "enforced"
	case GuardDevOpen:
		return "dev-open"
	case GuardHealthOnly:
		return "health-only"
	default:
		return "unknown"
	}
}
