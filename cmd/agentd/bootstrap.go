package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/maintainerd/agent/internal/coreclient"
	"github.com/maintainerd/agent/internal/grpcserver"
	"github.com/maintainerd/agent/internal/platform/config"
	"github.com/maintainerd/agent/internal/platform/logging"
	"github.com/maintainerd/agent/internal/runtimeclient"
	"github.com/maintainerd/agent/internal/server"
	"github.com/maintainerd/agent/internal/worker"
)

// version is the agent build version (overridable via -ldflags at build time).
var version = "0.1.0-dev"

// run executes the agent bootstrap: dial the runtime (docker) and, when
// CORE_ADDR is set, the control plane (core.v1), then serve the agent.v1 gRPC
// surface + HTTP liveness and run the pull → execute → report loop until a
// signal. The runtime and control-plane clients are agent-internal — the
// control plane is not a public SDK surface.
func run(parent context.Context) error {
	config.Load()
	logging.Setup(config.LogLevel)
	slog.Info("starting maintainerd-agent",
		"app_env", config.AppEnv,
		"secret_provider", config.SecretProvider,
		"agent_name", config.AgentName,
		"agent_uuid", config.AgentUUID,
		"runtime_addr", config.RuntimeAddr,
		"core_addr", config.CoreAddr,
		"grpc_port", config.GRPCPort,
		"http_port", config.HTTPPort,
	)

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	var conns []*grpc.ClientConn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	// Runtime (docker) — always.
	rtConn, err := grpc.NewClient(config.RuntimeAddr, dialOpts...)
	if err != nil {
		return fmt.Errorf("dial runtime: %w", err)
	}
	conns = append(conns, rtConn)
	rt := runtimeclient.New(rtConn)

	// Control plane (core.v1) — only when CORE_ADDR is set.
	var core *coreclient.Client
	if config.CoreAddr != "" {
		coreConn, err := grpc.NewClient(config.CoreAddr, dialOpts...)
		if err != nil {
			return fmt.Errorf("dial core: %w", err)
		}
		conns = append(conns, coreConn)
		core = coreclient.New(coreConn)
	}

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Probe the runtime. Warn but do not fail — the work loop retries.
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if err := rt.Ping(probeCtx); err != nil {
		slog.Warn("runtime (docker) not reachable yet", "addr", config.RuntimeAddr, "error", err.Error())
	} else {
		slog.Info("connected to runtime (docker)", "addr", config.RuntimeAddr)
	}
	cancel()

	if core != nil {
		slog.Info("control plane wired", "core_addr", config.CoreAddr)
	} else {
		slog.Warn("CORE_ADDR not set — running runtime-only (no control plane)")
	}

	// Best-effort register with Core (needs the agent to exist in Core's inventory).
	if core != nil && config.AgentUUID != "" {
		regCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := core.Register(regCtx, config.AgentUUID, version, nil); err != nil {
			slog.Warn("register with core failed (heartbeat will retry)", "error", err.Error())
		} else {
			slog.Info("registered with core", "agent_uuid", config.AgentUUID)
		}
		cancel()
	}

	agentSvc := grpcserver.NewService(config.AgentName, version, rt)
	httpSrv := server.New(rt)
	work := worker.New(core, rt, config.AgentUUID, config.PollInterval)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcserver.Serve(gctx, config.GRPCPort, agentSvc) })
	g.Go(func() error { return server.Start(gctx, config.HTTPPort, httpSrv.Router()) })
	g.Go(func() error { return work.Run(gctx) })
	return g.Wait()
}
