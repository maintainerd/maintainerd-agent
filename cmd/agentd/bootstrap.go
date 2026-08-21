package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/maintainerd/kit/log"
	kitserver "github.com/maintainerd/kit/server"
	sdk "github.com/maintainerd/sdk"
	sdkauth "github.com/maintainerd/sdk/auth"

	"github.com/maintainerd/agent/internal/coreclient"
	"github.com/maintainerd/agent/internal/grpcserver"
	"github.com/maintainerd/agent/internal/platform/config"
	dockerdriver "github.com/maintainerd/agent/internal/runtime/docker"
	"github.com/maintainerd/agent/internal/server"
	"github.com/maintainerd/agent/internal/statecache"
	"github.com/maintainerd/agent/internal/worker"
)

// version is the agent build version (overridable via -ldflags at build time).
var version = "0.1.0-dev"

// run executes the agent bootstrap: build the in-process docker driver, dial
// the control plane (when CORE_ADDR is set) with the agent's auth-principal
// credentials, then serve the guarded agent.v1 gRPC surface + HTTP probes and
// run the convergence worker until a signal.
//
// The security posture is resolved here, before anything listens or dials:
//   - INBOUND: outside development, missing verifier config means the gRPC
//     listener starts health-only (fail closed).
//   - OUTBOUND: outside development, a CORE_ADDR without token credentials
//     refuses to start at all (fail closed) — an anonymous agent channel
//     would let the host be enrolled without an identity.
func run(parent context.Context) error {
	config.Load()
	log.Setup(config.LogLevel)
	dev := config.IsDevelopment()
	slog.Info("starting maintainerd-agent",
		"app_env", config.AppEnv,
		"secret_provider", config.SecretProvider,
		"agent_name", config.AgentName,
		"agent_uuid", config.AgentUUID,
		"core_addr", config.CoreAddr,
		"grpc_port", config.GRPCPort,
		"http_port", config.HTTPPort,
		"state_dir", config.StateDir,
	)

	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Runtime driver — in-process docker (kubernetes later, same interface).
	rt, err := dockerdriver.NewDriver()
	if err != nil {
		return fmt.Errorf("build docker driver: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	if err := rt.Ping(probeCtx); err != nil {
		slog.Warn("docker engine not reachable yet — the worker retries", "error", err.Error())
	} else {
		slog.Info("connected to docker engine")
	}
	cancel()

	// Control plane (core.v1) — only when CORE_ADDR is set, always as an
	// authenticated principal outside development.
	var core *coreclient.Client
	if config.CoreAddr != "" {
		creds, err := buildCoreCredentials()
		if err != nil {
			return fmt.Errorf("core credentials: %w", err)
		}
		if creds == nil {
			if !dev {
				return fmt.Errorf("CORE_ADDR is set but no agent credentials are configured " +
					"(need AUTH_TOKEN_URL + AGENT_CLIENT_ID + AGENT_CLIENT_PRIVATE_KEY_FILE or AGENT_CLIENT_SECRET); " +
					"refusing to dial the control plane anonymously outside development")
			}
			slog.Warn("SECURITY: dialing core WITHOUT credentials (development only)",
				"disabled_guards", "agent identity token on core calls")
			creds = sdk.Anonymous{}
		}
		coreConn, err := grpc.NewClient(config.CoreAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			coreclient.WithCredentials(creds),
		)
		if err != nil {
			return fmt.Errorf("dial core: %w", err)
		}
		defer func() { _ = coreConn.Close() }()
		core = coreclient.New(coreConn)
		slog.Info("control plane wired", "core_addr", config.CoreAddr)
	}

	// Inbound guard for the agent's own gRPC surface.
	guard := resolveInboundGuard(ctx, dev)

	agentSvc := grpcserver.NewService(config.AgentName, version, rt)
	httpSrv := server.New(rt)
	cache := statecache.New(config.StateDir)

	var coreForWorker worker.CoreClient
	if core != nil {
		coreForWorker = core
	}
	work := worker.New(coreForWorker, rt, cache, worker.Options{
		AgentUUID:            config.AgentUUID,
		Version:              version,
		PollInterval:         config.PollInterval,
		HeartbeatInterval:    config.HeartbeatInterval,
		DriftInterval:        config.DriftInterval,
		ReconcileConcurrency: config.ReconcileConcurrency,
		OfflineThreshold:     config.OfflineThreshold,
	})

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcserver.Serve(gctx, config.GRPCPort, agentSvc, guard) })
	g.Go(func() error { return kitserver.ServeHTTP(gctx, config.HTTPPort, httpSrv.Router()) })
	g.Go(func() error { return work.Run(gctx) })
	return g.Wait()
}

// resolveInboundGuard decides how the gRPC listener treats callers. Outside
// development the verifier is mandatory: no JWKS/issuer/audience (or a JWKS
// that cannot be fetched) means the AgentService does not come up at all —
// an unauthenticated control surface on a fleet host is how one box becomes
// every box. In development, missing config degrades to an open listener
// with a loud warning naming every disabled guard.
func resolveInboundGuard(ctx context.Context, dev bool) grpcserver.Guard {
	missing := missingAuthVars()
	if len(missing) > 0 {
		reason := fmt.Sprintf("missing %v", missing)
		if dev {
			return grpcserver.Guard{Mode: grpcserver.GuardDevOpen, Reason: reason, Dev: true}
		}
		return grpcserver.Guard{Mode: grpcserver.GuardHealthOnly, Reason: reason}
	}
	verifier, err := sdkauth.NewVerifier(ctx, config.AuthJWKSURL, config.AuthIssuer, config.AuthAudience)
	if err != nil {
		reason := fmt.Sprintf("verifier init against %s failed: %v", config.AuthJWKSURL, err)
		if dev {
			return grpcserver.Guard{Mode: grpcserver.GuardDevOpen, Reason: reason, Dev: true}
		}
		return grpcserver.Guard{Mode: grpcserver.GuardHealthOnly, Reason: reason}
	}
	return grpcserver.Guard{Mode: grpcserver.GuardEnforced, Verify: grpcserver.SDKVerify(verifier), Dev: dev}
}

func missingAuthVars() []string {
	var missing []string
	if config.AuthJWKSURL == "" {
		missing = append(missing, "AUTH_JWKS_URL")
	}
	if config.AuthIssuer == "" {
		missing = append(missing, "AUTH_ISSUER")
	}
	if config.AuthAudience == "" {
		missing = append(missing, "AUTH_AUDIENCE")
	}
	return missing
}

// buildCoreCredentials constructs the agent's outbound identity. Private-key
// JWT is preferred: the key never leaves this host and a dump of auth's
// database cannot impersonate the agent. A client secret is the supported
// fallback. Returns (nil, nil) when no credential config is present — the
// caller decides whether that is fatal (production) or a loud dev warning.
func buildCoreCredentials() (sdk.Credentials, error) {
	if config.AuthTokenURL == "" || config.AgentClientID == "" {
		return nil, nil
	}
	if config.AgentClientPrivateKeyFile != "" {
		pem, err := os.ReadFile(config.AgentClientPrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read AGENT_CLIENT_PRIVATE_KEY_FILE: %w", err)
		}
		return sdk.PrivateKeyJWT(sdk.PrivateKeyJWTConfig{
			TokenEndpoint: config.AuthTokenURL,
			ClientID:      config.AgentClientID,
			PrivateKeyPEM: pem,
		})
	}
	if config.AgentClientSecret != "" {
		return sdk.ClientCredentials(sdk.ClientCredentialsConfig{
			TokenEndpoint: config.AuthTokenURL,
			ClientID:      config.AgentClientID,
			ClientSecret:  config.AgentClientSecret,
		})
	}
	return nil, nil
}
