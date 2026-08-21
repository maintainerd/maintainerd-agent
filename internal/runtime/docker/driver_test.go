package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/maintainerd/kit/runtime"
)

// fakeClient is an in-memory Docker Engine standing in for the SDK client.
type fakeClient struct {
	// engine state
	containers map[string]*fakeContainer // id → container
	images     map[string]bool           // ref → present
	networks   map[string]bool
	volumes    map[string]volume.CreateOptions

	// call recording
	pulls          []image.PullOptions
	pulledRefs     []string
	created        []createCall
	started        []string
	stopped        []container.StopOptions
	stoppedIDs     []string
	removed        []container.RemoveOptions
	removedIDs     []string
	netConnects    []string
	volumesRemoved []string
	listFilters    []string

	// failure injection
	pullErr   error
	createErr error
	startErr  error
}

type fakeContainer struct {
	id      string
	name    string
	image   string
	state   container.ContainerState
	labels  map[string]string
	health  *container.Health
	created int64
	tty     bool
}

type createCall struct {
	config   *container.Config
	hostCfg  *container.HostConfig
	netCfg   *network.NetworkingConfig
	platform *ocispec.Platform
	name     string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		containers: map[string]*fakeContainer{},
		images:     map[string]bool{},
		networks:   map[string]bool{},
		volumes:    map[string]volume.CreateOptions{},
	}
}

func (f *fakeClient) Ping(context.Context) (types.Ping, error) { return types.Ping{}, nil }

func (f *fakeClient) ImagePull(_ context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	f.pulls = append(f.pulls, opts)
	f.pulledRefs = append(f.pulledRefs, ref)
	f.images[ref] = true
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (f *fakeClient) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	if !f.images[ref] {
		return image.InspectResponse{}, fmt.Errorf("no such image %s: %w", ref, cerrdefs.ErrNotFound)
	}
	return image.InspectResponse{}, nil
}

func (f *fakeClient) ContainerCreate(_ context.Context, cfg *container.Config, hostCfg *container.HostConfig, netCfg *network.NetworkingConfig, platform *ocispec.Platform, name string) (container.CreateResponse, error) {
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	f.created = append(f.created, createCall{cfg, hostCfg, netCfg, platform, name})
	id := fmt.Sprintf("ctr-%d", len(f.created))
	f.containers[id] = &fakeContainer{
		id: id, name: name, image: cfg.Image, state: container.StateCreated, labels: cfg.Labels,
	}
	return container.CreateResponse{ID: id}, nil
}

func (f *fakeClient) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	if f.startErr != nil {
		return f.startErr
	}
	c, ok := f.containers[id]
	if !ok {
		return fmt.Errorf("no such container %s: %w", id, cerrdefs.ErrNotFound)
	}
	c.state = container.StateRunning
	f.started = append(f.started, id)
	return nil
}

func (f *fakeClient) ContainerStop(_ context.Context, id string, opts container.StopOptions) error {
	if c, ok := f.containers[id]; ok {
		c.state = container.StateExited
	}
	f.stopped = append(f.stopped, opts)
	f.stoppedIDs = append(f.stoppedIDs, id)
	return nil
}

func (f *fakeClient) ContainerRemove(_ context.Context, id string, opts container.RemoveOptions) error {
	delete(f.containers, id)
	f.removed = append(f.removed, opts)
	f.removedIDs = append(f.removedIDs, id)
	return nil
}

func (f *fakeClient) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	c, ok := f.containers[id]
	if !ok {
		return container.InspectResponse{}, fmt.Errorf("no such container %s: %w", id, cerrdefs.ErrNotFound)
	}
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:   c.id,
			Name: "/" + c.name,
			State: &container.State{
				Status:  c.state,
				Running: c.state == container.StateRunning,
				Health:  c.health,
			},
		},
		Config: &container.Config{Image: c.image, Labels: c.labels, Tty: c.tty},
	}, nil
}

func (f *fakeClient) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	f.listFilters = append(f.listFilters, strings.Join(opts.Filters.Get("label"), ","))
	var out []container.Summary
	for _, c := range f.containers {
		if !matchesLabelFilters(c.labels, opts.Filters.Get("label")) {
			continue
		}
		out = append(out, container.Summary{
			ID: c.id, Names: []string{"/" + c.name}, Image: c.image,
			State: c.state, Labels: c.labels, Created: c.created,
		})
	}
	return out, nil
}

func matchesLabelFilters(labels map[string]string, wanted []string) bool {
	for _, w := range wanted {
		k, v, hasValue := strings.Cut(w, "=")
		got, present := labels[k]
		if !present || (hasValue && got != v) {
			return false
		}
	}
	return true
}

func (f *fakeClient) ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (f *fakeClient) NetworkInspect(_ context.Context, name string, _ network.InspectOptions) (network.Inspect, error) {
	if !f.networks[name] {
		return network.Inspect{}, fmt.Errorf("no such network %s: %w", name, cerrdefs.ErrNotFound)
	}
	return network.Inspect{}, nil
}

func (f *fakeClient) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	f.networks[name] = true
	return network.CreateResponse{ID: "net-" + name}, nil
}

func (f *fakeClient) NetworkConnect(_ context.Context, networkID, containerID string, _ *network.EndpointSettings) error {
	f.netConnects = append(f.netConnects, networkID+":"+containerID)
	return nil
}

func (f *fakeClient) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	f.volumes[opts.Name] = opts
	return volume.Volume{Name: opts.Name}, nil
}

func (f *fakeClient) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.volumesRemoved = append(f.volumesRemoved, name)
	delete(f.volumes, name)
	return nil
}

// --- helpers ---

func baseSpec() runtime.WorkloadSpec {
	return runtime.WorkloadSpec{
		Image:  "example.com/app:1",
		Name:   "app",
		Labels: map[string]string{runtime.LabelResource: "res-1"},
	}
}

func noEnv(string) (string, bool) { return "", false }

// seedContainer plants a container owned by res-1 with the given spec's hash.
func seedContainer(t *testing.T, f *fakeClient, spec runtime.WorkloadSpec, state container.ContainerState) *fakeContainer {
	t.Helper()
	hash, err := spec.SpecHash()
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	c := &fakeContainer{
		id: "seeded", name: spec.Name, image: spec.Image, state: state,
		labels: map[string]string{runtime.LabelResource: spec.Labels[runtime.LabelResource], runtime.LabelSpecHash: hash},
	}
	f.containers[c.id] = c
	return c
}

// --- tests ---

func TestEnsureConverge(t *testing.T) {
	t.Run("absent: pulls, creates, starts, stamps labels", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, noEnv)
		spec := baseSpec()

		st, changed, err := d.Ensure(context.Background(), spec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if !changed {
			t.Error("changed = false, want true")
		}
		if len(f.pulledRefs) != 1 || f.pulledRefs[0] != spec.Image {
			t.Errorf("pulled %v, want [%s]", f.pulledRefs, spec.Image)
		}
		if len(f.created) != 1 || len(f.started) != 1 {
			t.Fatalf("created=%d started=%d, want 1/1", len(f.created), len(f.started))
		}
		labels := f.created[0].config.Labels
		if labels[runtime.LabelResource] != "res-1" {
			t.Errorf("resource label = %q, want res-1", labels[runtime.LabelResource])
		}
		wantHash, _ := spec.SpecHash()
		if labels[runtime.LabelSpecHash] != wantHash {
			t.Errorf("spec-hash label = %q, want %q", labels[runtime.LabelSpecHash], wantHash)
		}
		if !st.Running {
			t.Errorf("status.Running = false, want true")
		}
	})

	t.Run("present + hash match + running: no-op", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, noEnv)
		spec := baseSpec()
		seedContainer(t, f, spec, container.StateRunning)

		st, changed, err := d.Ensure(context.Background(), spec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if changed {
			t.Error("changed = true, want false")
		}
		if len(f.created) != 0 || len(f.started) != 0 || len(f.removedIDs) != 0 {
			t.Errorf("created=%d started=%d removed=%d, want all 0", len(f.created), len(f.started), len(f.removedIDs))
		}
		if st.ID != "seeded" {
			t.Errorf("status.ID = %q, want seeded", st.ID)
		}
	})

	t.Run("present + hash match + exited: starts, never replaces", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, noEnv)
		spec := baseSpec()
		seedContainer(t, f, spec, container.StateExited)

		st, changed, err := d.Ensure(context.Background(), spec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if !changed {
			t.Error("changed = false, want true")
		}
		if len(f.started) != 1 || f.started[0] != "seeded" {
			t.Errorf("started = %v, want [seeded]", f.started)
		}
		if len(f.created) != 0 || len(f.removedIDs) != 0 {
			t.Errorf("created=%d removed=%d, want 0/0", len(f.created), len(f.removedIDs))
		}
		if !st.Running {
			t.Error("status.Running = false, want true")
		}
	})

	t.Run("present + hash mismatch: stop, remove, recreate", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, noEnv)
		oldSpec := baseSpec()
		oldSpec.Image = "example.com/app:0"
		seedContainer(t, f, oldSpec, container.StateRunning)
		f.images["example.com/app:1"] = true

		newSpec := baseSpec()
		sig := "SIGQUIT"
		newSpec.StopSignal = sig

		_, changed, err := d.Ensure(context.Background(), newSpec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if !changed {
			t.Error("changed = false, want true")
		}
		if len(f.stoppedIDs) != 1 || f.stoppedIDs[0] != "seeded" {
			t.Fatalf("stopped = %v, want [seeded]", f.stoppedIDs)
		}
		if f.stopped[0].Signal != sig {
			t.Errorf("stop signal = %q, want %q", f.stopped[0].Signal, sig)
		}
		if len(f.removedIDs) != 1 || f.removedIDs[0] != "seeded" {
			t.Errorf("removed = %v, want [seeded]", f.removedIDs)
		}
		if len(f.created) != 1 {
			t.Errorf("created = %d, want 1", len(f.created))
		}
	})

	t.Run("missing ownership label: refused", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, noEnv)
		spec := baseSpec()
		spec.Labels = nil

		if _, _, err := d.Ensure(context.Background(), spec); err == nil {
			t.Fatal("Ensure accepted a spec without an ownership label")
		}
	})

	t.Run("invalid spec: refused before touching the engine", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, noEnv)
		spec := baseSpec()
		spec.Image = ""

		if _, _, err := d.Ensure(context.Background(), spec); err == nil {
			t.Fatal("Ensure accepted an invalid spec")
		}
		if len(f.pulledRefs)+len(f.created)+len(f.started) != 0 {
			t.Error("invalid spec reached the engine")
		}
	})
}

func TestEnsurePullPolicies(t *testing.T) {
	tests := []struct {
		name         string
		policy       runtime.PullPolicy
		imagePresent bool
		wantPulls    int
		wantErr      bool
	}{
		{"if-not-present skips when present", runtime.PullIfNotPresent, true, 0, false},
		{"if-not-present pulls when absent", runtime.PullIfNotPresent, false, 1, false},
		{"empty policy defaults to if-not-present", "", false, 1, false},
		{"always pulls even when present", runtime.PullAlways, true, 1, false},
		{"never fails when absent", runtime.PullNever, false, 0, true},
		{"never succeeds when present", runtime.PullNever, true, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeClient()
			d := newDriver(f, noEnv)
			spec := baseSpec()
			spec.PullPolicy = tt.policy
			if tt.imagePresent {
				f.images[spec.Image] = true
			}

			_, _, err := d.Ensure(context.Background(), spec)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if len(f.pulls) != tt.wantPulls {
				t.Errorf("pulls = %d, want %d", len(f.pulls), tt.wantPulls)
			}
		})
	}
}

func TestRegistryAuthResolution(t *testing.T) {
	creds := base64.StdEncoding.EncodeToString([]byte("bob:s3cret"))
	env := map[string]string{"AGENT_REGISTRY_AUTH_GHCR_PROD": creds}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	t.Run("resolves and encodes docker RegistryAuth", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, lookup)
		spec := baseSpec()
		spec.RegistryCredentialRef = "ghcr-prod"

		if _, _, err := d.Ensure(context.Background(), spec); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(f.pulls) != 1 {
			t.Fatalf("pulls = %d, want 1", len(f.pulls))
		}
		got, err := registry.DecodeAuthConfig(f.pulls[0].RegistryAuth)
		if err != nil {
			t.Fatalf("DecodeAuthConfig: %v", err)
		}
		if got.Username != "bob" || got.Password != "s3cret" {
			t.Errorf("auth = %s/%s, want bob/s3cret", got.Username, got.Password)
		}
	})

	t.Run("named but unconfigured credential fails closed", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, lookup)
		spec := baseSpec()
		spec.RegistryCredentialRef = "missing"

		_, _, err := d.Ensure(context.Background(), spec)
		if err == nil || !strings.Contains(err.Error(), "AGENT_REGISTRY_AUTH_MISSING") {
			t.Fatalf("err = %v, want missing-credential error naming the env var", err)
		}
		if len(f.pulls) != 0 {
			t.Error("pull attempted without the named credential")
		}
	})

	t.Run("garbage base64 fails closed", func(t *testing.T) {
		f := newFakeClient()
		d := newDriver(f, func(string) (string, bool) { return "not base64 !!", true })
		spec := baseSpec()
		spec.RegistryCredentialRef = "bad"

		if _, _, err := d.Ensure(context.Background(), spec); err == nil {
			t.Fatal("Ensure accepted invalid credential encoding")
		}
	})
}

func TestEnsureVolumesAndNetworks(t *testing.T) {
	f := newFakeClient()
	f.networks["existing-net"] = true
	d := newDriver(f, noEnv)
	spec := baseSpec()
	spec.Volumes = []runtime.VolumeSpec{{Name: "data", Labels: map[string]string{"x": "y"}}}
	spec.Networks = []runtime.NetworkAttachment{
		{Name: "existing-net", Aliases: []string{"app"}},
		{Name: "new-net"},
	}

	_, _, err := d.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	vol, ok := f.volumes["data"]
	if !ok {
		t.Fatal("named volume was not ensured")
	}
	if vol.Labels[runtime.LabelResource] != "res-1" || vol.Labels["x"] != "y" {
		t.Errorf("volume labels = %v, want resource label + spec labels", vol.Labels)
	}
	if !f.networks["new-net"] {
		t.Error("missing network was not created")
	}
	if len(f.created) != 1 {
		t.Fatalf("created = %d, want 1", len(f.created))
	}
	// First network at create; the rest connected afterwards.
	if _, ok := f.created[0].netCfg.EndpointsConfig["existing-net"]; !ok {
		t.Error("first network not in the create-time NetworkingConfig")
	}
	if len(f.netConnects) != 1 || !strings.HasPrefix(f.netConnects[0], "new-net:") {
		t.Errorf("netConnects = %v, want [new-net:<id>]", f.netConnects)
	}
}

func TestEnsureFieldMapping(t *testing.T) {
	f := newFakeClient()
	d := newDriver(f, noEnv)
	spec := baseSpec()
	spec.Env = map[string]string{"B": "2", "A": "1"}
	spec.Ports = []runtime.PortBinding{{ContainerPort: 8080, HostPort: 80, HostIP: "127.0.0.1", Protocol: "tcp"}}
	spec.Security = runtime.Security{ReadOnlyRootfs: true, NoNewPrivileges: true, CapDrop: []string{"ALL"}}
	spec.Resources = runtime.Resources{MemoryLimitBytes: 1 << 20, PidsLimit: 64}
	spec.Logging = runtime.Logging{Driver: "json-file", Options: map[string]string{"max-size": "1m"}}

	if _, _, err := d.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	cc := f.created[0]

	if want := []string{"A=1", "B=2"}; !equalStrings(cc.config.Env, want) {
		t.Errorf("env = %v, want sorted %v", cc.config.Env, want)
	}
	if _, ok := cc.config.ExposedPorts["8080/tcp"]; !ok {
		t.Errorf("exposed ports = %v, want 8080/tcp", cc.config.ExposedPorts)
	}
	pb := cc.hostCfg.PortBindings["8080/tcp"]
	if len(pb) != 1 || pb[0].HostPort != "80" || pb[0].HostIP != "127.0.0.1" {
		t.Errorf("port bindings = %v", pb)
	}
	if !cc.hostCfg.ReadonlyRootfs {
		t.Error("ReadonlyRootfs not set")
	}
	if !containsString(cc.hostCfg.SecurityOpt, "no-new-privileges") {
		t.Errorf("security opt = %v, want no-new-privileges appended", cc.hostCfg.SecurityOpt)
	}
	if cc.hostCfg.Resources.Memory != 1<<20 {
		t.Errorf("memory = %d, want %d", cc.hostCfg.Resources.Memory, 1<<20)
	}
	if cc.hostCfg.Resources.PidsLimit == nil || *cc.hostCfg.Resources.PidsLimit != 64 {
		t.Errorf("pids limit = %v, want 64", cc.hostCfg.Resources.PidsLimit)
	}
	if cc.hostCfg.LogConfig.Type != "json-file" || cc.hostCfg.LogConfig.Config["max-size"] != "1m" {
		t.Errorf("log config = %+v", cc.hostCfg.LogConfig)
	}
}

func TestEnsurePlatform(t *testing.T) {
	f := newFakeClient()
	d := newDriver(f, noEnv)
	spec := baseSpec()
	spec.Platform = "linux/arm64"

	if _, _, err := d.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p := f.created[0].platform
	if p == nil || p.OS != "linux" || p.Architecture != "arm64" {
		t.Errorf("platform = %+v, want linux/arm64", p)
	}
}

func TestListIsLabelScopedOnly(t *testing.T) {
	f := newFakeClient()
	f.containers["owned"] = &fakeContainer{id: "owned", name: "a", state: container.StateRunning,
		labels: map[string]string{runtime.LabelResource: "res-1"}}
	f.containers["foreign"] = &fakeContainer{id: "foreign", name: "b", state: container.StateRunning, labels: map[string]string{}}
	d := newDriver(f, noEnv)

	t.Run("empty labels refused", func(t *testing.T) {
		if _, err := d.List(context.Background(), nil); err == nil {
			t.Fatal("List with no labels must be refused — it would enumerate the host")
		}
	})

	t.Run("exact label match", func(t *testing.T) {
		got, err := d.List(context.Background(), map[string]string{runtime.LabelResource: "res-1"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != "owned" {
			t.Errorf("List = %v, want only the owned container", got)
		}
	})

	t.Run("empty value means label presence", func(t *testing.T) {
		got, err := d.List(context.Background(), map[string]string{runtime.LabelResource: ""})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != "owned" {
			t.Errorf("List = %v, want only the labeled container", got)
		}
	})
}

func TestStatusHealthMapping(t *testing.T) {
	tests := []struct {
		name   string
		health *container.Health
		want   runtime.HealthState
	}{
		{"no healthcheck", nil, runtime.HealthNone},
		{"healthy", &container.Health{Status: container.Healthy}, runtime.HealthHealthy},
		{"unhealthy", &container.Health{Status: container.Unhealthy}, runtime.HealthUnhealthy},
		{"starting", &container.Health{Status: container.Starting}, runtime.HealthStarting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeClient()
			f.containers["c1"] = &fakeContainer{id: "c1", name: "app", state: container.StateRunning, health: tt.health}
			d := newDriver(f, noEnv)

			st, err := d.Status(context.Background(), "c1")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if st.Health != tt.want {
				t.Errorf("health = %q, want %q", st.Health, tt.want)
			}
		})
	}
}

func TestRemoveVolumesPerCall(t *testing.T) {
	t.Run("removeVolumes=false leaves named volumes", func(t *testing.T) {
		f := newFakeClient()
		f.containers["c1"] = &fakeContainer{id: "c1", name: "app", state: container.StateExited}
		d := newDriver(f, noEnv)

		if err := d.Remove(context.Background(), "c1", false); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if len(f.removed) != 1 || !f.removed[0].Force {
			t.Errorf("removed = %+v, want one force remove", f.removed)
		}
		if f.removed[0].RemoveVolumes {
			t.Error("RemoveVolumes was set without the call asking for it")
		}
		if len(f.volumesRemoved) != 0 {
			t.Errorf("volumes removed = %v, want none", f.volumesRemoved)
		}
	})

	t.Run("removeVolumes=true forwards the flag", func(t *testing.T) {
		f := newFakeClient()
		f.containers["c1"] = &fakeContainer{id: "c1", name: "app", state: container.StateExited}
		d := newDriver(f, noEnv)

		if err := d.Remove(context.Background(), "c1", true); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		if !f.removed[0].RemoveVolumes {
			t.Error("RemoveVolumes flag not forwarded")
		}
	})
}

func TestEnvKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ghcr-prod", "GHCR_PROD"},
		{"simple", "SIMPLE"},
		{"dots.and/slashes", "DOTS_AND_SLASHES"},
		{"UPPER123", "UPPER123"},
	}
	for _, tt := range tests {
		if got := envKey(tt.in); got != tt.want {
			t.Errorf("envKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSpecHashChangesOnFieldChange(t *testing.T) {
	// Guards the convergence identity: any spec change must change the hash,
	// otherwise Ensure would consider a stale container converged.
	a := baseSpec()
	b := baseSpec()
	b.Env = map[string]string{"NEW": "value"}
	ha, _ := a.SpecHash()
	hb, _ := b.SpecHash()
	if ha == hb {
		t.Error("spec hash did not change when the spec changed")
	}
	// Sanity: hashes are stable for identical specs.
	ha2, _ := baseSpec().SpecHash()
	if ha != ha2 {
		t.Error("spec hash is not deterministic")
	}
	var decoded runtime.WorkloadSpec
	blob, _ := json.Marshal(a)
	_ = json.Unmarshal(blob, &decoded)
	hd, _ := decoded.SpecHash()
	if ha != hd {
		t.Error("spec hash changed across a JSON round-trip")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
