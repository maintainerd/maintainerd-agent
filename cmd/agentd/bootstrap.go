package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/maintainerd/agent/internal/grpcserver"
	"github.com/maintainerd/agent/internal/platform/config"
	"github.com/maintainerd/agent/internal/platform/logging"
	"github.com/maintainerd/agent/internal/runtimeclient"
	"github.com/maintainerd/agent/internal/server"
	"github.com/maintainerd/agent/internal/worker"
)

// version is the agent build version (overridable via -ldflags at build time).
var version = "0.1.0-dev"

// run executes the agent bootstrap: connect to the runtime (docker), then serve
// the agent.v1 gRPC surface + HTTP liveness and run the work loop until a signal.
func run(parent context.Context) error {
	config.Load()
	logging.Setup(config.LogLevel)
	slog.Info("starting maintainerd-agent",
		"app_env", config.AppEnv,
		"agent_name", config.AgentName,
		"runtime_addr", config.RuntimeAddr,
		"grpc_port", config.GRPCPort,
		"http_port", config.HTTPPort,
	)

	rt, err := runtimeclient.Dial(config.RuntimeAddr)
	if err != nil {
		return fmt.Errorf("dial runtime: %w", err)
	}
	defer rt.Close()

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Probe the runtime (docker). Warn but do not fail — it may come up later,
	// and the work loop retries every tick.
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if err := rt.Ping(probeCtx); err != nil {
		slog.Warn("runtime (docker) not reachable yet", "addr", config.RuntimeAddr, "error", err.Error())
	} else {
		slog.Info("connected to runtime (docker)", "addr", config.RuntimeAddr)
	}
	cancel()

	agentSvc := grpcserver.NewService(config.AgentName, version, rt)
	httpSrv := server.New(rt)
	work := worker.New(rt, config.PollInterval)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcserver.Serve(gctx, config.GRPCPort, agentSvc) })
	g.Go(func() error { return server.Start(gctx, config.HTTPPort, httpSrv.Router()) })
	g.Go(func() error { return work.Run(gctx) })
	return g.Wait()
}
