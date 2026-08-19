// Package config loads maintainerd-agent's configuration from the environment.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
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
	AppEnv = getEnv("APP_ENV", "development")
	LogLevel = getEnv("LOG_LEVEL", "info")
	SecretProvider = getEnv("SECRET_PROVIDER", "env")
	AgentName = getEnv("AGENT_NAME", "agent-local")
	RuntimeAddr = getEnv("RUNTIME_ADDR", "localhost:9090")
	CoreAddr = getEnv("CORE_ADDR", "")
	AgentUUID = getEnv("AGENT_UUID", "")
	GRPCPort = normalizePort(getEnv("GRPC_PORT", "9091"))
	HTTPPort = normalizePort(getEnv("HTTP_PORT", "8091"))
	PollInterval = getDuration("POLL_INTERVAL", 5*time.Second)
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Duration(secs) * time.Second
		}
	}
	return def
}

func normalizePort(p string) string {
	if !strings.HasPrefix(p, ":") {
		return ":" + p
	}
	return p
}
