// Package grpcserver serves the agent.v1.AgentService contract this repo owns:
// the agent's local control/health surface.
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

	sdkruntime "github.com/maintainerd/agent/internal/runtimeclient"

	agentv1 "github.com/maintainerd/agent/gen/maintainerd/agent/v1"
)

// Service implements agentv1.AgentServiceServer.
type Service struct {
	agentv1.UnimplementedAgentServiceServer
	name    string
	version string
	rt      *sdkruntime.Client
}

func NewService(name, version string, rt *sdkruntime.Client) *Service {
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

// Serve starts the gRPC server (AgentService + gRPC health + reflection) and
// stops it gracefully when ctx is cancelled.
func Serve(ctx context.Context, addr string, svc *Service) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(gs, svc)

	hs := health.NewServer()
	hs.SetServingStatus("maintainerd.agent.v1.AgentService", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hs)
	reflection.Register(gs)

	go func() {
		<-ctx.Done()
		slog.Info("shutting down grpc server")
		gs.GracefulStop()
	}()

	slog.Info("grpc server listening", "addr", addr)
	if err := gs.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
