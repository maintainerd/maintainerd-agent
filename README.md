# maintainerd-agent

The **per-host executor** for the maintainerd control plane. It **dials OUT to
Core** (`core.v1`) — never the reverse — pulls desired state, and converges
container workloads through a **compiled-in runtime driver** (docker today; a
kubernetes driver lands later behind the same `kit/runtime.Runtime` interface).
When Core is unreachable it keeps the **system tier** (auth, secret, core
itself) alive from an on-disk cache. Its inbound surface is minimal and
**governed by maintainerd-auth**: tokens are verified with the sdk and
enforced per method.

## Architecture

- **Outbound-only posture** — the agent dials Core; Core never reaches into a
  host. Outside development the agent refuses to dial Core without token
  credentials (private-key JWT preferred, client secret fallback), and every
  RPC carries its identity token.
- **In-process docker driver** (`internal/runtime/docker`) — implements
  `kit/runtime.Runtime`. `Ensure` converges by ownership label
  (`maintainerd.resource`) + spec hash (`maintainerd.spec-hash`): absent →
  pull/create/start; hash match → keep (start if exited); mismatch → graceful
  stop, remove, recreate. `List` is label-scoped only — it never enumerates
  the host. Registry credentials resolve driver-side from
  `AGENT_REGISTRY_AUTH_<REF>` env vars; they never travel in specs.
- **Convergence worker** (`internal/worker`) — decoupled loops: heartbeat
  (never blocked by reconciles), pull + bounded-concurrency reconcile, drift
  observation (reports exited/unhealthy transitions), registration with
  backoff + re-register on reconnect.
- **Static cache** (`internal/statecache`) — system-tier work items persist
  atomically to `STATE_DIR`. After `OFFLINE_THRESHOLD` consecutive failed
  pulls the agent supervises cached system items offline until Core returns;
  non-system work pauses.
- **Guarded gRPC surface** (`internal/grpcserver`) — `AgentService`
  (`Ping`/`Info` on `:9091`) behind sdk token verification and a
  method→permission map (`Info` → `agent:info:read`). Outside development,
  missing auth config means the listener starts **health-only**. Reflection
  is development-only. HTTP `/healthz` + `/readyz` stay unauthenticated
  (probe endpoints — the only exception).

## Spec envelope

`WorkItem.spec_json` parses as:

```json
{"workload": { /* kit runtime.WorkloadSpec */ }, "tier": "system", "teardown": false}
```

The legacy bare `{image,name,cmd,env}` shape is still accepted for
compatibility with today's Core; Core adopts the envelope next phase.
`teardown: true` removes the resource's workloads (honoring
`remove_volumes_on_teardown`). Invalid specs are reported `failed`, never
guessed at.

## Layout

- `proto/maintainerd/agent/v1` + `gen/…` — the owned `AgentService` contract
- `internal/runtime/docker` — the compiled-in docker driver (`kit/runtime.Runtime`)
- `internal/worker` — pull → converge → report loops + offline supervision
- `internal/statecache` — atomic on-disk cache of system-tier work
- `internal/coreclient` — Core `AgentGateway` client + per-RPC agent credentials
- `internal/grpcserver` — guarded `AgentService` (+ gRPC health; reflection in dev)
- `internal/server` — HTTP `/healthz` + `/readyz`
- `cmd/agentd` — the entrypoint

## Multi-repo note

The agent consumes `maintainerd` (core protos), `maintainerd-kit`,
`maintainerd-sdk`, and `maintainerd-secret` via local module replaces
(`go.mod`), developed side by side. CI checks out those siblings and repoints
the replaces.

## Run

```bash
# development: docker engine on the host, no auth required
APP_ENV=development make run
grpcurl -plaintext localhost:9091 maintainerd.agent.v1.AgentService/Info
curl localhost:8091/readyz     # 200 when the docker engine is reachable
```

## Config (env)

| Var | Default | Purpose |
|-----|---------|---------|
| `APP_ENV` | `development` | anything but `development` gets fail-closed semantics |
| `LOG_LEVEL` | `info` | log level |
| `SECRET_PROVIDER` | `env` | secret source |
| `AGENT_NAME` | `agent-local` | agent identity |
| `AGENT_UUID` | (empty) | this agent's identity in Core |
| `CORE_ADDR` | (empty) | Core control plane; empty = runtime-only mode |
| `GRPC_PORT` | `9091` | guarded `AgentService` |
| `HTTP_PORT` | `8091` | HTTP liveness/readiness |
| `POLL_INTERVAL` | `5s` | work-pull cadence (also offline supervision cadence) |
| `HEARTBEAT_INTERVAL` | `10s` | heartbeat cadence (own goroutine) |
| `DRIFT_INTERVAL` | `30s` | workload re-observation cadence |
| `RECONCILE_CONCURRENCY` | `4` | bounded reconcile pool size |
| `OFFLINE_THRESHOLD` | `3` | consecutive pull failures before offline supervision |
| `STATE_DIR` | `/var/lib/maintainerd-agent` | system-tier cache (0700/0600) |
| `AUTH_JWKS_URL` | (empty) | inbound token verification — all three required outside dev |
| `AUTH_ISSUER` | (empty) | expected token issuer |
| `AUTH_AUDIENCE` | (empty) | expected token audience |
| `AUTH_TOKEN_URL` | (empty) | auth token endpoint for the agent's own identity |
| `AGENT_CLIENT_ID` | (empty) | the agent's OAuth2 client |
| `AGENT_CLIENT_PRIVATE_KEY_FILE` | (empty) | private-key JWT credential (preferred) |
| `AGENT_CLIENT_SECRET` | (empty) | client-secret credential (fallback) |
| `AGENT_REGISTRY_AUTH_<REF>` | (empty) | `base64(user:password)` for a spec's `registry_credential_ref` |
