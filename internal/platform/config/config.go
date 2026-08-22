// Package config loads maintainerd-agent's configuration from the environment.
// The shared env helpers and base config (AppEnv/LogLevel/SecretProvider) come
// from maintainerd-kit so the agent stops re-implementing them; only the
// agent-specific fields live here.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	kitconfig "github.com/maintainerd/kit/config"
)

var (
	// AppEnv is "development" or "production". Every security default in the
	// agent keys off this: anything that is not exactly "development" gets
	// production (fail-closed) semantics, so a typo'd APP_ENV can never
	// accidentally disable a guard.
	AppEnv string
	// LogLevel is debug|info|warn|error.
	LogLevel string
	// SecretProvider selects where secrets come from (SECRET_PROVIDER, default
	// "env"). maintainerd-secret and cloud providers plug in here as they land.
	SecretProvider string
	// AgentName identifies this agent instance.
	AgentName string
	// CoreAddr is the maintainerd-core control-plane gRPC address the agent
	// dials OUT to for work (core.v1). Empty = runtime-only mode (no control
	// plane). The agent always dials core; core never dials the agent.
	CoreAddr string
	// AgentUUID is this agent's identity in Core (its agent_uuid), used for
	// Register/Heartbeat/PullWork/ReportStatus.
	AgentUUID string
	// AgentJoinToken is the one-time enrollment token issued by Core. When
	// present and no local client certificate exists, startup uses it to obtain
	// the per-agent mTLS certificate before normal gateway calls begin.
	AgentJoinToken string
	// AgentClientCertFile / AgentClientKeyFile hold the enrolled per-agent
	// client certificate and private key used for Core gateway mTLS.
	AgentClientCertFile string
	AgentClientKeyFile  string
	// AgentClientCAFile stores the CA certificate returned by enrollment. It
	// documents the issuer of the local client cert and is available for later
	// renewal/inspection workflows.
	AgentClientCAFile string
	// CoreTLSCAFile / CoreTLSServerName configure server authentication for
	// the agent's outbound Core gRPC channel. Outside development, Core dials
	// must use TLS.
	CoreTLSCAFile     string
	CoreTLSServerName string
	// GRPCPort is the listen address for this agent's AgentService (e.g. ":9091").
	GRPCPort string
	// HTTPPort is the listen address for the HTTP liveness surface (e.g. ":8091").
	HTTPPort string

	// PollInterval is how often the work loop pulls from Core (POLL_INTERVAL,
	// default 5s). Offline supervision reconciles cached system items on the
	// same cadence.
	PollInterval time.Duration
	// HeartbeatInterval is how often the agent heartbeats Core
	// (HEARTBEAT_INTERVAL, default 10s). Heartbeats run on their own goroutine
	// so a slow reconcile (e.g. a large image pull) can never make Core think
	// the host is down.
	HeartbeatInterval time.Duration
	// DriftInterval is how often the agent re-observes its own workloads and
	// reports state transitions (DRIFT_INTERVAL, default 30s). This is what
	// makes a reported "running" stay honest after creation.
	DriftInterval time.Duration
	// ReconcileConcurrency bounds how many work items converge at once
	// (RECONCILE_CONCURRENCY, default 4). Bounded so one slow pull cannot
	// serialize the batch, and so the agent cannot stampede the engine.
	ReconcileConcurrency int
	// OfflineThreshold is how many consecutive pull failures flip the agent
	// into offline supervision of the cached system tier (OFFLINE_THRESHOLD,
	// default 3).
	OfflineThreshold int
	// StateDir is where the system-tier work cache persists (STATE_DIR,
	// default /var/lib/maintainerd-agent). Owner-only on disk — specs may
	// embed configuration secrets.
	StateDir string

	// AuthJWKSURL / AuthIssuer / AuthAudience configure INBOUND token
	// verification for the agent's own gRPC surface (the sdk verifier).
	// Outside development all three are required or the gRPC listener starts
	// health-only — an unauthenticated control surface on a fleet host is how
	// one box becomes every box.
	AuthJWKSURL  string
	AuthIssuer   string
	AuthAudience string

	// AuthTokenURL + AgentClientID (+ key file or secret) configure the
	// agent's OUTBOUND identity: the OAuth2 client it authenticates to Core
	// as. Private-key JWT (AGENT_CLIENT_PRIVATE_KEY_FILE) is preferred — the
	// key never leaves this host; a shared secret (AGENT_CLIENT_SECRET) is
	// the fallback.
	AuthTokenURL              string
	AgentClientID             string
	AgentClientPrivateKeyFile string
	AgentClientSecret         string
)

// Load populates the package-level config from the environment. Call once at startup.
func Load() {
	base := kitconfig.LoadBase()
	AppEnv = base.AppEnv
	LogLevel = base.LogLevel
	SecretProvider = base.SecretProvider
	AgentName = kitconfig.GetEnv("AGENT_NAME", "agent-local")
	CoreAddr = kitconfig.GetEnv("CORE_ADDR", "")
	AgentUUID = kitconfig.GetEnv("AGENT_UUID", "")
	GRPCPort = kitconfig.NormalizePort(kitconfig.GetEnv("GRPC_PORT", "9091"))
	HTTPPort = kitconfig.NormalizePort(kitconfig.GetEnv("HTTP_PORT", "8091"))

	PollInterval = kitconfig.GetDuration("POLL_INTERVAL", 5*time.Second)
	HeartbeatInterval = kitconfig.GetDuration("HEARTBEAT_INTERVAL", 10*time.Second)
	DriftInterval = kitconfig.GetDuration("DRIFT_INTERVAL", 30*time.Second)
	ReconcileConcurrency = getInt("RECONCILE_CONCURRENCY", 4)
	OfflineThreshold = getInt("OFFLINE_THRESHOLD", 3)
	StateDir = kitconfig.GetEnv("STATE_DIR", "/var/lib/maintainerd-agent")
	identityDir := filepath.Join(StateDir, "identity")
	AgentJoinToken = kitconfig.GetEnv("AGENT_JOIN_TOKEN", "")
	AgentClientCertFile = kitconfig.GetEnv("AGENT_CLIENT_CERT_FILE", filepath.Join(identityDir, "agent.crt"))
	AgentClientKeyFile = kitconfig.GetEnv("AGENT_CLIENT_KEY_FILE", filepath.Join(identityDir, "agent.key"))
	AgentClientCAFile = kitconfig.GetEnv("AGENT_CLIENT_CA_FILE", filepath.Join(identityDir, "agent-ca.crt"))
	CoreTLSCAFile = kitconfig.GetEnv("CORE_TLS_CA_FILE", "")
	CoreTLSServerName = kitconfig.GetEnv("CORE_TLS_SERVER_NAME", "")

	AuthJWKSURL = kitconfig.GetEnv("AUTH_JWKS_URL", "")
	AuthIssuer = kitconfig.GetEnv("AUTH_ISSUER", "")
	AuthAudience = kitconfig.GetEnv("AUTH_AUDIENCE", "")

	AuthTokenURL = kitconfig.GetEnv("AUTH_TOKEN_URL", "")
	AgentClientID = kitconfig.GetEnv("AGENT_CLIENT_ID", "")
	AgentClientPrivateKeyFile = kitconfig.GetEnv("AGENT_CLIENT_PRIVATE_KEY_FILE", "")
	AgentClientSecret = kitconfig.GetEnv("AGENT_CLIENT_SECRET", "")
}

// IsDevelopment reports whether the agent runs with development semantics.
// Anything else — including an empty or misspelled APP_ENV — is production:
// guards fail closed on configuration mistakes.
func IsDevelopment() bool { return AppEnv == "development" }

// getInt parses an int env var, keeping the default on unset or garbage — a
// malformed knob should degrade to the safe default, not crash the fleet agent.
func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
