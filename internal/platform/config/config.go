// Package config loads maintainerd-agent's configuration from the environment.
// The shared env helpers and base config (AppEnv/LogLevel/SecretProvider) come
// from maintainerd-kit so the agent stops re-implementing them; only the
// agent-specific fields live here.
package config

import (
	"time"

	kitconfig "github.com/maintainerd/kit/config"
)

var (
	// AppEnv is "development" or "production".
	AppEnv string
	// LogLevel is debug|info|warn|error.
	LogLevel string
	// SecretProvider selects where secrets come from (SECRET_PROVIDER, default
	// "env"). maintainerd-secret and cloud providers plug in here as they land.
	SecretProvider string
	// AgentName identifies this agent instance.
	AgentName string
	// RuntimeAddr is the maintainerd-docker RuntimeService gRPC address.
	RuntimeAddr string
	// CoreAddr is the maintainerd-core control-plane gRPC address the agent pulls
	// work from (core.v1). Empty = runtime-only mode (no control plane).
	CoreAddr string
	// AgentUUID is this agent's identity in Core (its agent_uuid), used for
	// Register/Heartbeat/PullWork/ReportStatus.
	AgentUUID string
	// GRPCPort is the listen address for this agent's AgentService (e.g. ":9091").
	GRPCPort string
	// HTTPPort is the listen address for the HTTP liveness surface (e.g. ":8091").
	HTTPPort string
	// PollInterval is how often the work loop pulls from Core.
	PollInterval time.Duration
)

// Load populates the package-level config from the environment. Call once at startup.
func Load() {
	base := kitconfig.LoadBase()
	AppEnv = base.AppEnv
	LogLevel = base.LogLevel
	SecretProvider = base.SecretProvider
	AgentName = kitconfig.GetEnv("AGENT_NAME", "agent-local")
	RuntimeAddr = kitconfig.GetEnv("RUNTIME_ADDR", "localhost:9090")
	CoreAddr = kitconfig.GetEnv("CORE_ADDR", "")
	AgentUUID = kitconfig.GetEnv("AGENT_UUID", "")
	GRPCPort = kitconfig.NormalizePort(kitconfig.GetEnv("GRPC_PORT", "9091"))
	HTTPPort = kitconfig.NormalizePort(kitconfig.GetEnv("HTTP_PORT", "8091"))
	PollInterval = kitconfig.GetDuration("POLL_INTERVAL", 5*time.Second)
}
