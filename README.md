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
- `packaging/systemd` — the unit + annotated environment template (the
  production install; see *Running under systemd*)

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
| `AGENT_UUID` | (empty) | this agent's identity in Core (required with `CORE_ADDR`) |
| `CORE_ADDR` | (empty) | Core control plane. **Required outside development** — empty selects runtime-only mode, which is a development affordance |
| `AGENT_JOIN_TOKEN` | (empty) | one-time enrollment token (**secret**; one boot only) |
| `AGENT_CLIENT_CERT_FILE` | `$STATE_DIR/identity/agent.crt` | enrolled mTLS client certificate |
| `AGENT_CLIENT_KEY_FILE` | `$STATE_DIR/identity/agent.key` | its private key (**secret**, 0600) |
| `AGENT_CLIENT_CA_FILE` | `$STATE_DIR/identity/agent-ca.crt` | CA that issued the client certificate |
| `CORE_TLS_CA_FILE` | (empty) | CA for Core's server certificate; TLS is required outside dev |
| `CORE_TLS_SERVER_NAME` | (empty) | override the TLS server name when it differs from the dial host |
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

## Running under systemd

In docker mode the agent runs as a **binary under systemd**, not as a
container. The reason is circular dependency: the agent's job includes
converging and repairing workloads on the local docker engine, so the engine
must not be the thing that keeps the agent alive. A containerized agent cannot
restart the daemon it lives in, and it dies exactly when the host needs it most.
The precedent is uniform — kubelet, containerd, k3s, the Nomad client and the
Consul agent all run under the init system; control-plane components run as
containers. (In kubernetes mode the agent instead runs as an in-cluster
Deployment, **one per cluster**, because the API is cluster-scoped rather than
per-node. That driver is not shipped yet.)

`packaging/systemd/` holds the unit and an annotated environment template. The
unit runs as an unprivileged `maintainerd` user with `docker` as a supplementary
group, `ProtectSystem=strict`, no capabilities, and one writable directory. Read
the comments in it before editing — every directive there has a stated reason,
including the honest note that `docker` group membership is root-equivalent
through the socket and this unit does not sandbox that away.

### Install

```bash
# 1. Fetch the release for the host's architecture and VERIFY it.
V=v0.1.0; A=amd64
curl -fsSLO https://github.com/maintainerd/maintainerd-agent/releases/download/$V/maintainerd-agent_${V}_linux_${A}.tar.gz
curl -fsSLO https://github.com/maintainerd/maintainerd-agent/releases/download/$V/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS     # must print OK before anything below runs as root
tar -xzf maintainerd-agent_${V}_linux_${A}.tar.gz && cd maintainerd-agent_${V}_linux_${A}

# 2. Service user. No login shell, no home: it exists to own one directory.
sudo useradd --system --no-create-home --shell /usr/sbin/nologin maintainerd
sudo usermod -aG docker maintainerd

# 3. Binary + unit.
sudo install -m 0755 maintainerd-agent /usr/local/bin/maintainerd-agent
sudo install -m 0644 maintainerd-agent.service /etc/systemd/system/

# 4. Environment file — root-owned, 0600, because it carries the join token.
sudo install -d -m 0755 /etc/maintainerd
sudo install -m 0600 agent.env.example /etc/maintainerd/agent.env
sudo editor /etc/maintainerd/agent.env          # AGENT_UUID, CORE_ADDR, AGENT_JOIN_TOKEN, AUTH_*

sudo systemctl daemon-reload
sudo systemctl enable --now maintainerd-agent
```

`systemd-analyze security maintainerd-agent.service` scores the shipped unit at
**1.5 (OK)** on systemd 255; the remaining exposure is network access, the
docker supplementary group, and `PrivateDevices=` left off (see the note in the
unit).

### State and identity

`StateDirectory=maintainerd-agent` makes systemd create
`/var/lib/maintainerd-agent` as `maintainerd:maintainerd` mode `0700` on every
start, and it is the **only** writable path in the unit. It holds:

- `identity/agent.key` + `identity/agent.crt` (`0600`) — the enrolled mTLS
  client identity for the Core gateway.
- `identity/agent-ca.crt` — the CA that issued it.
- the system-tier work cache, which is what keeps auth, secret and core itself
  supervised while Core is unreachable. Work specs can embed configuration
  secrets, hence the mode.

Nothing else on the host is written. `journalctl` is the only log sink; there is
no file to rotate or leave world-readable.

### Enrollment (first boot only)

1. **On Core**: `CreateAgent` returns an `agent_uuid` and a **one-time join
   token**.
2. **In the environment file**: set `AGENT_UUID` and `AGENT_JOIN_TOKEN`. The
   token belongs in `/etc/maintainerd/agent.env` and **never** on a command
   line — a command line is readable by every user on the box through
   `/proc/<pid>/cmdline`, so `ps` would print the credential to anyone logged
   in. systemd reads the file as PID 1 before dropping privileges, so the
   service's own uid never needs read access to it either.
3. **First start**: the agent generates an EC key and a CSR **locally** (the
   private key never leaves the host, and Core never sees it), calls `Enroll`
   with the token, and writes the returned certificate and CA under the state
   directory.
4. **Delete `AGENT_JOIN_TOKEN`** from the environment file once
   `journalctl -u maintainerd-agent | grep enrolled` shows the exchange
   succeeded. Core will not honour it twice, so a token left behind is a leaked
   credential with no remaining use.
5. **Later boots** skip enrollment entirely: the certificate on disk is the
   identity. A half-written identity (cert without key, or key without cert) is
   a hard boot error rather than a silent re-enrollment, because re-enrolling
   would burn a token that has already been spent.

**What this does not solve.** Join tokens are single-use, so an autoscaling
group cannot scale on its own — something has to pre-create one token per
instance and deliver it to that instance's user-data. This is a real
limitation, not a configuration detail. `kubeadm` avoids it with TTL-bounded
reusable tokens; k3s uses one shared cluster token. Both trade blast radius for
elasticity. The better answer is cloud attestation (IMDS identity documents,
GCP instance identity tokens, TPM attestation) so a host proves what it *is*
rather than presenting a secret somebody had to hand it — the enrollment RPC is
the seam where that would land, and it is not built.

### Upgrade

```bash
# Verify the new tarball as above, then:
sudo systemctl stop maintainerd-agent
sudo install -m 0755 maintainerd-agent /usr/local/bin/maintainerd-agent
sudo systemctl start maintainerd-agent
journalctl -u maintainerd-agent -n 20    # the boot line reports the stamped version
```

The state directory survives, so an upgrade never re-enrolls. Workloads the
agent owns keep running while it is stopped — it converges them, it does not
parent them — and the drift loop re-observes and re-reports them on the next
tick. Replace `maintainerd-agent.service` and `daemon-reload` only when the unit
itself changed.

The version in that boot line is stamped at link time (`-X`
`…/internal/platform/config.AppVersion`), not read from the environment: a
version an operator can set is a version that can disagree with the binary. It
is also what the agent reports to Core on `Register` and returns from the
`Info` RPC, so the fleet view and the journal cannot drift apart.

### Logs

```bash
journalctl -u maintainerd-agent -f                     # follow
journalctl -u maintainerd-agent -p warning --since -1h # warnings and errors only
journalctl -u maintainerd-agent -o cat | jq .          # structured: the agent logs JSON
systemctl status maintainerd-agent                     # restart count, uptime, exit codes
```

Two failure shapes worth knowing by name. `CORE_ADDR is required outside
development` means the agent **refused to start**: a host with no control-plane
address converges nothing while still answering `/healthz`, so it fails at boot
rather than pretending. `gRPC AgentService REFUSING to start — serving health
only` means inbound token verification is unconfigured (`AUTH_JWKS_URL`, `AUTH_ISSUER`,
`AUTH_AUDIENCE` are a set, all three or none) — the agent keeps converging, but
its guarded surface is closed.
