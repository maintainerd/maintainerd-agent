package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

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

// run executes the agent bootstrap: connect to the runtime (docker) and, if
// configured, to Core (core.v1); then serve the agent.v1 gRPC surface + HTTP
// liveness and run the pull → execute → report loop until a signal.
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

	rt, err := runtimeclient.Dial(config.RuntimeAddr)
	if err != nil {
		return fmt.Errorf("dial runtime: %w", err)
	}
	defer rt.Close()

	// Core is optional: with no CORE_ADDR the agent runs runtime-only.
	var core *coreclient.Client
	if config.CoreAddr != "" {
		core, err = coreclient.Dial(config.CoreAddr)
		if err != nil {
			return fmt.Errorf("dial core: %w", err)
		}
		defer core.Close()
		slog.Info("control plane wired", "core_addr", config.CoreAddr)
	} else {
		slog.Warn("CORE_ADDR not set — running runtime-only (no control plane)")
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
