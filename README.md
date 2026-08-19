# maintainerd-agent

The **on-host executor** for the maintainerd control plane. It is the "hands":
it **pulls work from Core** (`core.v1`, the control plane) and **executes it
against the local runtime** (`maintainerd-docker`'s `RuntimeService`), then
reports observed status back. Core never reaches into a host directly — the
agent dials out and pulls.

## gRPC ownership

- **Owns** `maintainerd.agent.v1.AgentService` — the agent's local control/health
  surface (served on `:9091`).
- **Client of** `maintainerd.runtime.v1.RuntimeService` (owned by
  `maintainerd-docker`) — imports its generated stubs.
- **Client of** `maintainerd.core.v1` (owned by `maintainerd-core`) — the
  work-pull protocol. *Reserved — not yet wired.*

## Layout

- `proto/maintainerd/agent/v1` + `gen/…` — the owned `AgentService` contract
- `internal/runtimeclient` — client to `maintainerd-docker`'s `RuntimeService`
- `internal/worker` — the pull → execute → report loop (Core wiring is stubbed)
- `internal/grpcserver` — serves `AgentService` (+ gRPC health + reflection)
- `internal/server` — HTTP liveness (`/healthz`) + readiness (`/readyz`, checks runtime)
- `internal/platform/{config,logging}`
- `cmd/agentd` — the entrypoint

## Multi-repo note

The agent consumes `maintainerd-docker`'s stubs via a local module replace
(`go.mod`: `replace github.com/maintainerd/docker => ../maintainerd-docker`),
so the two repos are developed side by side. A published module / Go workspace
replaces this for CI and container builds.

## Run

```bash
# with maintainerd-docker running on :9090
make run
grpcurl -plaintext localhost:9091 maintainerd.agent.v1.AgentService/Info
curl localhost:8091/readyz     # 200 when the runtime (docker) is reachable
```

## Config (env)

| Var | Default | Purpose |
|-----|---------|---------|
| `APP_ENV` | `development` | environment |
| `LOG_LEVEL` | `info` | log level |
| `AGENT_NAME` | `agent-local` | agent identity |
| `RUNTIME_ADDR` | `localhost:9090` | maintainerd-docker RuntimeService |
| `CORE_ADDR` | (empty) | maintainerd-core control plane (reserved) |
| `GRPC_PORT` | `9091` | this agent's `AgentService` |
| `HTTP_PORT` | `8091` | HTTP liveness/readiness |
| `POLL_INTERVAL` | `5s` | work-loop interval |
