// Package docker implements the kit runtime.Runtime contract against the
// local Docker Engine. It is compiled into the agent — the driver runs where
// the engine lives, and the only inbound surface it adds to the host is the
// engine's own unix socket, which never leaves the machine. A kubernetes
// driver implements the same interface later; callers never see the engine.
package docker

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/maintainerd/kit/runtime"
)

// registryAuthEnvPrefix is where the driver resolves registry credentials.
// A spec carries only an opaque RegistryCredentialRef NAME; the matching
// secret lives in the agent's environment as
// AGENT_REGISTRY_AUTH_<upper(ref)> = base64("user:password").
// Credentials never travel in specs: specs are persisted in Core's database
// and shipped over the wire, so a name is inert where a secret is a leak.
const registryAuthEnvPrefix = "AGENT_REGISTRY_AUTH_"

// Driver drives a single Docker Engine through the kit runtime contract.
// It is safe for concurrent use (the underlying SDK client is).
type Driver struct {
	cli dockerClient
	// lookupEnv resolves registry credential refs; injectable for tests.
	lookupEnv func(string) (string, bool)
}

// Compile-time check that Driver satisfies the kit runtime contract.
var _ runtime.Runtime = (*Driver)(nil)

// NewDriver connects to the Docker Engine using standard environment
// resolution (DOCKER_HOST, TLS env, or the default unix socket) and negotiates
// the API version with the daemon so it works across engine versions.
func NewDriver() (*Driver, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker: create client: %w", err)
	}
	return &Driver{cli: cli, lookupEnv: os.LookupEnv}, nil
}

// newDriver wires an arbitrary client — the test seam.
func newDriver(cli dockerClient, lookupEnv func(string) (string, bool)) *Driver {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &Driver{cli: cli, lookupEnv: lookupEnv}
}

// Ping verifies the engine is reachable.
func (d *Driver) Ping(ctx context.Context) error {
	if _, err := d.cli.Ping(ctx); err != nil {
		return fmt.Errorf("docker: ping engine: %w", err)
	}
	return nil
}

// Ensure converges the engine toward the spec:
//
//   - absent                     → pull (per policy) + create + start
//   - present, spec-hash match   → ensure running (start if exited); never replace
//   - present, spec-hash differs → stop (spec signal/timeout) + remove + recreate
//
// Identity is the runtime.LabelResource label — Ensure refuses a spec without
// it, because without an ownership label the driver cannot tell its own
// containers from foreign ones and a converge could adopt (or destroy)
// something it does not own. Fail closed beats guessing.
func (d *Driver) Ensure(ctx context.Context, spec runtime.WorkloadSpec) (runtime.WorkloadStatus, bool, error) {
	if err := spec.Validate(); err != nil {
		return runtime.WorkloadStatus{}, false, err
	}
	resourceID := spec.Labels[runtime.LabelResource]
	if resourceID == "" {
		return runtime.WorkloadStatus{}, false, fmt.Errorf("docker: spec is missing the %s ownership label", runtime.LabelResource)
	}
	specHash, err := spec.SpecHash()
	if err != nil {
		return runtime.WorkloadStatus{}, false, err
	}

	existing, err := d.findOwned(ctx, resourceID)
	if err != nil {
		return runtime.WorkloadStatus{}, false, err
	}

	if existing != nil && existing.Labels[runtime.LabelSpecHash] == specHash {
		// Desired state already materialized; converge only the run state.
		if existing.State == container.StateRunning {
			st, err := d.Status(ctx, existing.ID)
			return st, false, err
		}
		if err := d.cli.ContainerStart(ctx, existing.ID, container.StartOptions{}); err != nil {
			return runtime.WorkloadStatus{}, false, fmt.Errorf("docker: start container %s: %w", existing.ID, err)
		}
		st, err := d.Status(ctx, existing.ID)
		return st, true, err
	}

	if existing != nil {
		// Hash mismatch: replace. Stop with the spec's signal/timeout so the
		// old workload gets its graceful shutdown before removal.
		if err := d.stopWithSpec(ctx, existing.ID, spec); err != nil {
			return runtime.WorkloadStatus{}, false, err
		}
		if err := d.cli.ContainerRemove(ctx, existing.ID, container.RemoveOptions{Force: true}); err != nil {
			return runtime.WorkloadStatus{}, false, fmt.Errorf("docker: remove container %s: %w", existing.ID, err)
		}
	}

	id, err := d.create(ctx, spec, resourceID, specHash)
	if err != nil {
		return runtime.WorkloadStatus{}, false, err
	}
	st, err := d.Status(ctx, id)
	return st, true, err
}

// Stop gracefully stops a running workload with the engine's default timeout.
func (d *Driver) Stop(ctx context.Context, id string) error {
	if err := d.cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		return fmt.Errorf("docker: stop container %s: %w", id, err)
	}
	return nil
}

// Remove force-removes a workload. removeVolumes additionally deletes the
// named volumes it mounted — data destruction is opt-in per call, never a
// side effect, so a teardown can't silently take user data with it.
func (d *Driver) Remove(ctx context.Context, id string, removeVolumes bool) error {
	var namedVolumes []string
	if removeVolumes {
		// Collect the named volumes BEFORE removal; ContainerRemove's own
		// RemoveVolumes only covers anonymous volumes.
		if resp, err := d.cli.ContainerInspect(ctx, id); err == nil {
			for _, m := range resp.Mounts {
				if m.Type == mount.TypeVolume && m.Name != "" {
					namedVolumes = append(namedVolumes, m.Name)
				}
			}
		}
	}
	if err := d.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: removeVolumes}); err != nil {
		return fmt.Errorf("docker: remove container %s: %w", id, err)
	}
	for _, name := range namedVolumes {
		if err := d.cli.VolumeRemove(ctx, name, false); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("docker: remove volume %s: %w", name, err)
		}
	}
	return nil
}

// Status returns the observed state of a single workload.
func (d *Driver) Status(ctx context.Context, id string) (runtime.WorkloadStatus, error) {
	resp, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return runtime.WorkloadStatus{}, fmt.Errorf("docker: inspect container %s: %w", id, err)
	}
	st := runtime.WorkloadStatus{Health: runtime.HealthNone}
	if resp.ContainerJSONBase != nil {
		st.ID = resp.ID
		st.Name = strings.TrimPrefix(resp.Name, "/")
		st.CreatedAt = parseDockerTime(resp.Created)
		if resp.State != nil {
			st.State = resp.State.Status
			st.Running = resp.State.Running
			st.ExitCode = resp.State.ExitCode
			st.Error = resp.State.Error
			st.StartedAt = parseDockerTime(resp.State.StartedAt)
			st.FinishedAt = parseDockerTime(resp.State.FinishedAt)
			st.Health = mapHealth(resp.State.Health)
		}
	}
	if resp.Config != nil {
		st.Image = resp.Config.Image
	}
	return st, nil
}

// List returns ONLY workloads carrying the given labels. An empty label set
// is refused: enumerating the whole host would leak containers the agent does
// not own into the control plane, so the ownership scoping is enforced here,
// not left to callers. A map entry with an empty value matches label PRESENCE
// (any value) — how the worker lists everything it owns across resources.
func (d *Driver) List(ctx context.Context, labels map[string]string) ([]runtime.WorkloadStatus, error) {
	if len(labels) == 0 {
		return nil, fmt.Errorf("docker: List requires at least one label (label-scoped by contract; the driver never enumerates the host)")
	}
	f := filters.NewArgs()
	for k, v := range labels {
		if v == "" {
			f.Add("label", k)
		} else {
			f.Add("label", k+"="+v)
		}
	}
	items, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	out := make([]runtime.WorkloadStatus, 0, len(items))
	for _, c := range items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, runtime.WorkloadStatus{
			ID:        c.ID,
			Name:      name,
			Image:     c.Image,
			State:     c.State,
			Running:   c.State == container.StateRunning,
			Health:    runtime.HealthNone, // Summary has no health detail; use Status for it
			CreatedAt: time.Unix(c.Created, 0).UTC(),
		})
	}
	return out, nil
}

// Logs streams a workload's logs. Docker multiplexes stdout/stderr on one
// stream for non-TTY containers; we demux via stdcopy into a single combined
// reader so callers never see the raw multiplex framing.
func (d *Driver) Logs(ctx context.Context, id string, opts runtime.LogOptions) (io.ReadCloser, error) {
	tail := "all"
	if opts.TailLines > 0 {
		tail = strconv.Itoa(opts.TailLines)
	}
	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     opts.Follow,
		Tail:       tail,
		Timestamps: opts.Timestamps,
	})
	if err != nil {
		return nil, fmt.Errorf("docker: logs for container %s: %w", id, err)
	}
	if resp, err := d.cli.ContainerInspect(ctx, id); err == nil && resp.Config != nil && resp.Config.Tty {
		return rc, nil // TTY containers produce an unmultiplexed stream already
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = rc.Close()
		_ = pw.CloseWithError(err)
	}()
	return &pipeWithSource{PipeReader: pr, src: rc}, nil
}

// pipeWithSource closes the underlying docker stream when the caller closes
// the demuxed reader, so a Follow stream doesn't leak the HTTP connection.
type pipeWithSource struct {
	*io.PipeReader
	src io.Closer
}

func (p *pipeWithSource) Close() error {
	_ = p.src.Close()
	return p.PipeReader.Close()
}

// --- convergence internals ---

// findOwned locates the container owned by the resource, by label only.
func (d *Driver) findOwned(ctx context.Context, resourceID string) (*container.Summary, error) {
	f := filters.NewArgs(filters.Arg("label", runtime.LabelResource+"="+resourceID))
	items, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("docker: find container for resource %s: %w", resourceID, err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	// Deterministic pick if duplicates ever exist (e.g. a crashed replace):
	// the newest wins; the stale ones get replaced away on hash mismatch.
	sort.Slice(items, func(i, j int) bool { return items[i].Created > items[j].Created })
	return &items[0], nil
}

// stopWithSpec stops honoring the spec's StopSignal/StopTimeout.
func (d *Driver) stopWithSpec(ctx context.Context, id string, spec runtime.WorkloadSpec) error {
	opts := container.StopOptions{Signal: spec.StopSignal}
	if spec.StopTimeout != nil {
		secs := int(spec.StopTimeout.Round(time.Second) / time.Second)
		opts.Timeout = &secs
	}
	if err := d.cli.ContainerStop(ctx, id, opts); err != nil {
		return fmt.Errorf("docker: stop container %s: %w", id, err)
	}
	return nil
}

// create materializes the spec: image (per pull policy), named volumes,
// networks, container, extra network attachments, then start.
func (d *Driver) create(ctx context.Context, spec runtime.WorkloadSpec, resourceID, specHash string) (string, error) {
	if err := d.ensureImage(ctx, spec); err != nil {
		return "", err
	}
	if err := d.ensureVolumes(ctx, spec, resourceID); err != nil {
		return "", err
	}
	if err := d.ensureNetworks(ctx, spec, resourceID); err != nil {
		return "", err
	}

	cfg, hostCfg, err := buildConfigs(spec, resourceID, specHash)
	if err != nil {
		return "", err
	}
	var netCfg *network.NetworkingConfig
	if len(spec.Networks) > 0 {
		first := spec.Networks[0]
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			first.Name: {Aliases: first.Aliases},
		}}
	}
	platform, err := parsePlatform(spec.Platform)
	if err != nil {
		return "", err
	}

	created, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, platform, spec.Name)
	if err != nil {
		return "", fmt.Errorf("docker: create container %q: %w", spec.Name, err)
	}
	for _, n := range spec.Networks[min(1, len(spec.Networks)):] {
		if err := d.cli.NetworkConnect(ctx, n.Name, created.ID, &network.EndpointSettings{Aliases: n.Aliases}); err != nil {
			return created.ID, fmt.Errorf("docker: connect container %s to network %q: %w", created.ID, n.Name, err)
		}
	}
	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return created.ID, fmt.Errorf("docker: start container %s: %w", created.ID, err)
	}
	return created.ID, nil
}

// ensureImage applies the pull policy. PullNever fails closed when the image
// is absent — running an unexpected best-effort pull under a "never" policy
// would defeat air-gapped / pre-vetted-image deployments.
func (d *Driver) ensureImage(ctx context.Context, spec runtime.WorkloadSpec) error {
	policy := spec.PullPolicy
	if policy == "" {
		policy = runtime.PullIfNotPresent
	}
	switch policy {
	case runtime.PullNever:
		if _, err := d.cli.ImageInspect(ctx, spec.Image); err != nil {
			if cerrdefs.IsNotFound(err) {
				return fmt.Errorf("docker: image %q absent and pull policy is never", spec.Image)
			}
			return fmt.Errorf("docker: inspect image %q: %w", spec.Image, err)
		}
		return nil
	case runtime.PullIfNotPresent:
		if _, err := d.cli.ImageInspect(ctx, spec.Image); err == nil {
			return nil
		} else if !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("docker: inspect image %q: %w", spec.Image, err)
		}
		return d.pull(ctx, spec)
	case runtime.PullAlways:
		return d.pull(ctx, spec)
	default:
		return fmt.Errorf("docker: unknown pull policy %q", policy)
	}
}

func (d *Driver) pull(ctx context.Context, spec runtime.WorkloadSpec) error {
	opts := image.PullOptions{}
	if spec.RegistryCredentialRef != "" {
		auth, err := d.resolveRegistryAuth(spec.RegistryCredentialRef)
		if err != nil {
			return err
		}
		opts.RegistryAuth = auth
	}
	rc, err := d.cli.ImagePull(ctx, spec.Image, opts)
	if err != nil {
		return fmt.Errorf("docker: pull %q: %w", spec.Image, err)
	}
	defer func() { _ = rc.Close() }()
	// The pull is asynchronous on the wire; draining to EOF is what blocks
	// until the image is fully present locally.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("docker: drain pull stream for %q: %w", spec.Image, err)
	}
	return nil
}

// resolveRegistryAuth turns an opaque credential NAME into docker RegistryAuth.
// The secret is looked up driver-side from the environment — never from the
// spec — and a named-but-missing credential is an error, not an anonymous
// pull: silently degrading to anonymous would surface as a confusing 401/404
// from the registry and could pull a same-named public image instead of the
// intended private one.
func (d *Driver) resolveRegistryAuth(ref string) (string, error) {
	key := registryAuthEnvPrefix + envKey(ref)
	raw, ok := d.lookupEnv(key)
	if !ok || raw == "" {
		return "", fmt.Errorf("docker: registry credential ref %q named by the spec but %s is not set", ref, key)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("docker: %s is not valid base64: %w", key, err)
	}
	user, pass, found := strings.Cut(string(decoded), ":")
	if !found || user == "" {
		return "", fmt.Errorf("docker: %s must decode to \"user:password\"", key)
	}
	encoded, err := registry.EncodeAuthConfig(registry.AuthConfig{Username: user, Password: pass})
	if err != nil {
		return "", fmt.Errorf("docker: encode registry auth: %w", err)
	}
	return encoded, nil
}

// envKey normalizes a credential ref into an env-var-safe suffix: uppercased,
// with every non [A-Z0-9] rune replaced by '_' (e.g. "ghcr-prod" → "GHCR_PROD").
func envKey(ref string) string {
	up := strings.ToUpper(ref)
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, up)
}

// ensureVolumes ensure-creates the spec's named volumes, stamped with the
// resource label so ownership stays traceable. VolumeCreate is idempotent for
// an existing name.
func (d *Driver) ensureVolumes(ctx context.Context, spec runtime.WorkloadSpec, resourceID string) error {
	for _, v := range spec.Volumes {
		labels := map[string]string{runtime.LabelResource: resourceID}
		for k, val := range v.Labels {
			labels[k] = val
		}
		if _, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: v.Name, Labels: labels}); err != nil {
			return fmt.Errorf("docker: ensure volume %q: %w", v.Name, err)
		}
	}
	return nil
}

// ensureNetworks creates each named network that does not exist yet as a
// labeled bridge network.
func (d *Driver) ensureNetworks(ctx context.Context, spec runtime.WorkloadSpec, resourceID string) error {
	for _, n := range spec.Networks {
		_, err := d.cli.NetworkInspect(ctx, n.Name, network.InspectOptions{})
		if err == nil {
			continue
		}
		if !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("docker: inspect network %q: %w", n.Name, err)
		}
		if _, err := d.cli.NetworkCreate(ctx, n.Name, network.CreateOptions{
			Driver: "bridge",
			Labels: map[string]string{runtime.LabelResource: resourceID},
		}); err != nil {
			return fmt.Errorf("docker: create network %q: %w", n.Name, err)
		}
	}
	return nil
}

// buildConfigs maps the mode-neutral spec onto docker's Config/HostConfig.
func buildConfigs(spec runtime.WorkloadSpec, resourceID, specHash string) (*container.Config, *container.HostConfig, error) {
	// Env sorted for determinism — the container sees a stable environment
	// regardless of Go map iteration order.
	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)

	// Labels: the spec's own, then the ownership + convergence labels the
	// driver stamps. Driver-stamped labels win any collision — a spec must
	// not be able to spoof its ownership or hash.
	labels := make(map[string]string, len(spec.Labels)+2)
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels[runtime.LabelResource] = resourceID
	labels[runtime.LabelSpecHash] = specHash

	exposed, bindings, err := buildPorts(spec.Ports)
	if err != nil {
		return nil, nil, err
	}

	cfg := &container.Config{
		Image:        spec.Image,
		Env:          env,
		Entrypoint:   spec.Entrypoint,
		Cmd:          spec.Cmd,
		Labels:       labels,
		WorkingDir:   spec.WorkingDir,
		User:         spec.User,
		ExposedPorts: exposed,
		StopSignal:   spec.StopSignal,
	}
	if spec.StopTimeout != nil {
		secs := int(spec.StopTimeout.Round(time.Second) / time.Second)
		cfg.StopTimeout = &secs
	}
	if spec.Health != nil {
		cfg.Healthcheck = &container.HealthConfig{
			Test:        spec.Health.Test,
			Interval:    spec.Health.Interval,
			Timeout:     spec.Health.Timeout,
			Retries:     spec.Health.Retries,
			StartPeriod: spec.Health.StartPeriod,
		}
	}

	mounts, err := buildMounts(spec.Mounts)
	if err != nil {
		return nil, nil, err
	}

	hostCfg := &container.HostConfig{
		PortBindings: bindings,
		Mounts:       mounts,
		RestartPolicy: container.RestartPolicy{
			Name:              container.RestartPolicyMode(spec.RestartPolicy.Name),
			MaximumRetryCount: spec.RestartPolicy.MaximumRetryCount,
		},
		ReadonlyRootfs: spec.Security.ReadOnlyRootfs,
		CapAdd:         strslice.StrSlice(spec.Security.CapAdd),
		CapDrop:        strslice.StrSlice(spec.Security.CapDrop),
		SecurityOpt:    append([]string(nil), spec.Security.SecurityOpt...),
	}
	// no-new-privileges blocks setuid/setgid escalation inside the workload;
	// appended by the driver so the flag can't be lost in translation.
	if spec.Security.NoNewPrivileges {
		hostCfg.SecurityOpt = append(hostCfg.SecurityOpt, "no-new-privileges")
	}
	hostCfg.Resources.CPUShares = spec.Resources.CPUShares
	hostCfg.Resources.CPUQuota = spec.Resources.CPUQuota
	hostCfg.Resources.Memory = spec.Resources.MemoryLimitBytes
	hostCfg.Resources.MemoryReservation = spec.Resources.MemoryReservationBytes
	if spec.Resources.PidsLimit > 0 {
		pids := spec.Resources.PidsLimit
		hostCfg.Resources.PidsLimit = &pids
	}
	if spec.Logging.Driver != "" || len(spec.Logging.Options) > 0 {
		hostCfg.LogConfig = container.LogConfig{Type: spec.Logging.Driver, Config: spec.Logging.Options}
	}
	return cfg, hostCfg, nil
}

func buildPorts(ports []runtime.PortBinding) (nat.PortSet, nat.PortMap, error) {
	if len(ports) == 0 {
		return nil, nil, nil
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for _, p := range ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		port, err := nat.NewPort(proto, strconv.Itoa(p.ContainerPort))
		if err != nil {
			return nil, nil, fmt.Errorf("docker: port %d/%s: %w", p.ContainerPort, proto, err)
		}
		exposed[port] = struct{}{}
		hostPort := ""
		if p.HostPort > 0 {
			hostPort = strconv.Itoa(p.HostPort)
		}
		bindings[port] = append(bindings[port], nat.PortBinding{HostIP: p.HostIP, HostPort: hostPort})
	}
	return exposed, bindings, nil
}

func buildMounts(mounts []runtime.Mount) ([]mount.Mount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]mount.Mount, 0, len(mounts))
	for _, m := range mounts {
		var mt mount.Type
		switch m.Type {
		case runtime.MountBind:
			mt = mount.TypeBind
		case runtime.MountVolume:
			mt = mount.TypeVolume
		case runtime.MountTmpfs:
			mt = mount.TypeTmpfs
		default:
			return nil, fmt.Errorf("docker: unsupported mount type %q", m.Type)
		}
		out = append(out, mount.Mount{Type: mt, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	return out, nil
}

// parsePlatform parses the spec's "os/arch" into an OCI platform.
func parsePlatform(p string) (*ocispec.Platform, error) {
	if p == "" {
		return nil, nil
	}
	osName, arch, found := strings.Cut(p, "/")
	if !found || osName == "" || arch == "" {
		return nil, fmt.Errorf("docker: platform %q is not os/arch", p)
	}
	return &ocispec.Platform{OS: osName, Architecture: arch}, nil
}

// mapHealth maps docker's health readback onto the runtime contract.
func mapHealth(h *container.Health) runtime.HealthState {
	if h == nil {
		return runtime.HealthNone
	}
	switch h.Status {
	case container.Healthy:
		return runtime.HealthHealthy
	case container.Unhealthy:
		return runtime.HealthUnhealthy
	case container.Starting:
		return runtime.HealthStarting
	default:
		return runtime.HealthNone
	}
}

// parseDockerTime parses docker's RFC3339Nano timestamps; the zero value is
// returned for empty/"zero" docker times per the runtime contract.
func parseDockerTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	if t.Unix() <= 0 && t.Year() <= 1 {
		return time.Time{}
	}
	return t
}
