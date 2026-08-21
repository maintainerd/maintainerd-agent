package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	kitruntime "github.com/maintainerd/kit/runtime"

	"github.com/maintainerd/agent/internal/coreclient"
	"github.com/maintainerd/agent/internal/statecache"
)

// --- fakes ---

type fakeCore struct {
	mu           sync.Mutex
	registers    int
	heartbeats   int
	pullErr      error
	pullItems    []coreclient.WorkItem
	pulls        int
	reports      []coreclient.StatusReport
	registerErrs int // fail the first N registrations
}

func (f *fakeCore) Register(_ context.Context, _, _ string, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registers++
	if f.registers <= f.registerErrs {
		return errors.New("register refused")
	}
	return nil
}

func (f *fakeCore) Heartbeat(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats++
	return nil
}

func (f *fakeCore) PullWork(context.Context, string, int32) ([]coreclient.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return f.pullItems, nil
}

func (f *fakeCore) ReportStatus(_ context.Context, _ string, reports []coreclient.StatusReport) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, reports...)
	return int32(len(reports)), nil
}

func (f *fakeCore) reportedStates() map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for _, r := range f.reports {
		out[r.ResourceUUID] = append(out[r.ResourceUUID], r.State)
	}
	return out
}

type ensureCall struct {
	spec kitruntime.WorkloadSpec
}

type fakeRuntime struct {
	mu        sync.Mutex
	ensures   []ensureCall
	removes   []string
	ensureErr error
	panicOn   string // spec name that panics
	statuses  map[string]kitruntime.WorkloadStatus
	listOut   []kitruntime.WorkloadStatus
}

func (f *fakeRuntime) Ping(context.Context) error { return nil }

func (f *fakeRuntime) Ensure(_ context.Context, spec kitruntime.WorkloadSpec) (kitruntime.WorkloadStatus, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if spec.Name == f.panicOn {
		panic("poisoned spec: " + spec.Name)
	}
	f.ensures = append(f.ensures, ensureCall{spec})
	if f.ensureErr != nil {
		return kitruntime.WorkloadStatus{}, false, f.ensureErr
	}
	id := "ctr-" + spec.Labels[kitruntime.LabelResource]
	return kitruntime.WorkloadStatus{ID: id, Name: spec.Name, State: "running", Running: true, Health: kitruntime.HealthNone}, true, nil
}

func (f *fakeRuntime) Stop(context.Context, string) error { return nil }

func (f *fakeRuntime) Remove(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, id)
	return nil
}

func (f *fakeRuntime) Status(_ context.Context, id string) (kitruntime.WorkloadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st, ok := f.statuses[id]; ok {
		return st, nil
	}
	return kitruntime.WorkloadStatus{ID: id, State: "running", Running: true, Health: kitruntime.HealthNone}, nil
}

func (f *fakeRuntime) List(context.Context, map[string]string) ([]kitruntime.WorkloadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listOut, nil
}

func (f *fakeRuntime) Logs(context.Context, string, kitruntime.LogOptions) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeRuntime) ensuredNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.ensures {
		out = append(out, c.spec.Name)
	}
	return out
}

// --- helpers ---

func envelopeJSON(t *testing.T, spec kitruntime.WorkloadSpec, tier string, teardown bool) string {
	t.Helper()
	blob, err := json.Marshal(map[string]any{"workload": spec, "tier": tier, "teardown": teardown})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(blob)
}

func newTestWorker(core CoreClient, rt kitruntime.Runtime, cache *statecache.Cache) *Worker {
	return New(core, rt, cache, Options{AgentUUID: "agent-1", Version: "test"})
}

// --- envelope parsing ---

func TestParseEnvelope(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantErr      bool
		wantImage    string
		wantTier     string
		wantTeardown bool
	}{
		{
			name:      "envelope shape",
			in:        `{"workload":{"image":"app:1","name":"app"},"tier":"system"}`,
			wantImage: "app:1",
			wantTier:  "system",
		},
		{
			name:         "envelope teardown without workload body",
			in:           `{"teardown":true}`,
			wantTeardown: true,
		},
		{
			name:      "legacy bare shape",
			in:        `{"image":"legacy:1","name":"old","cmd":["run"],"env":{"A":"1"}}`,
			wantImage: "legacy:1",
		},
		{name: "empty", in: "", wantErr: true},
		{name: "not json", in: "{nope", wantErr: true},
		{name: "neither shape", in: `{"foo":"bar"}`, wantErr: true},
		{name: "workload not a spec", in: `{"workload":[1,2]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := parseEnvelope(tt.in)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if env.Workload.Image != tt.wantImage {
				t.Errorf("image = %q, want %q", env.Workload.Image, tt.wantImage)
			}
			if env.Tier != tt.wantTier {
				t.Errorf("tier = %q, want %q", env.Tier, tt.wantTier)
			}
			if env.Teardown != tt.wantTeardown {
				t.Errorf("teardown = %v, want %v", env.Teardown, tt.wantTeardown)
			}
		})
	}
}

// --- reconcile ---

func TestReconcileEnsuresAndReports(t *testing.T) {
	core := &fakeCore{}
	rt := &fakeRuntime{}
	w := newTestWorker(core, rt, nil)

	spec := kitruntime.WorkloadSpec{Image: "app:1", Name: "app"}
	w.reconcileItem(context.Background(), coreclient.WorkItem{
		ResourceUUID: "res-1", Name: "app", Generation: 7,
		SpecJSON: envelopeJSON(t, spec, "", false),
	}, true)

	if got := rt.ensuredNames(); len(got) != 1 || got[0] != "app" {
		t.Fatalf("ensured = %v, want [app]", got)
	}
	// Ownership label injected by the worker, not trusted from the spec.
	if lbl := rt.ensures[0].spec.Labels[kitruntime.LabelResource]; lbl != "res-1" {
		t.Errorf("ownership label = %q, want res-1", lbl)
	}
	if len(core.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(core.reports))
	}
	r := core.reports[0]
	if r.State != "running" || r.ObservedGeneration != 7 {
		t.Errorf("report = %+v, want running at generation 7", r)
	}
	if !strings.Contains(r.StatusJSON, "ctr-res-1") {
		t.Errorf("status JSON %q lacks container id", r.StatusJSON)
	}
}

func TestReconcileInvalidSpecReportsFailed(t *testing.T) {
	tests := []struct {
		name string
		spec string
	}{
		{"unparseable", "{nope"},
		{"fails validation", envelopeJSONRaw(`{"image":"","name":"x"}`)},
		{"legacy without image", `{"name":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core := &fakeCore{}
			rt := &fakeRuntime{}
			w := newTestWorker(core, rt, nil)

			w.reconcileItem(context.Background(), coreclient.WorkItem{ResourceUUID: "res-x", Generation: 1, SpecJSON: tt.spec}, true)

			if len(rt.ensures) != 0 {
				t.Error("invalid spec reached the runtime")
			}
			if len(core.reports) != 1 || core.reports[0].State != "failed" {
				t.Fatalf("reports = %+v, want one failed", core.reports)
			}
			if !strings.Contains(core.reports[0].StatusJSON, "error") {
				t.Errorf("failed report %q carries no error", core.reports[0].StatusJSON)
			}
		})
	}
}

func envelopeJSONRaw(workload string) string {
	return fmt.Sprintf(`{"workload":%s}`, workload)
}

func TestReconcileTeardown(t *testing.T) {
	core := &fakeCore{}
	rt := &fakeRuntime{listOut: []kitruntime.WorkloadStatus{{ID: "ctr-old", Running: true}}}
	w := newTestWorker(core, rt, nil)

	w.reconcileItem(context.Background(), coreclient.WorkItem{
		ResourceUUID: "res-1", Generation: 3,
		SpecJSON: `{"teardown":true}`,
	}, true)

	if len(rt.removes) != 1 || rt.removes[0] != "ctr-old" {
		t.Fatalf("removes = %v, want [ctr-old]", rt.removes)
	}
	if len(core.reports) != 1 || core.reports[0].State != "removed" {
		t.Fatalf("reports = %+v, want one removed", core.reports)
	}
	if core.reports[0].ObservedGeneration != 3 {
		t.Errorf("generation = %d, want 3", core.reports[0].ObservedGeneration)
	}
}

func TestReconcileEnsureErrorReportsFailed(t *testing.T) {
	core := &fakeCore{}
	rt := &fakeRuntime{ensureErr: errors.New("engine on fire")}
	w := newTestWorker(core, rt, nil)

	w.reconcileItem(context.Background(), coreclient.WorkItem{
		ResourceUUID: "res-1", Generation: 1,
		SpecJSON: envelopeJSON(t, kitruntime.WorkloadSpec{Image: "app:1", Name: "app"}, "", false),
	}, true)

	if len(core.reports) != 1 || core.reports[0].State != "failed" {
		t.Fatalf("reports = %+v, want one failed", core.reports)
	}
	if !strings.Contains(core.reports[0].StatusJSON, "engine on fire") {
		t.Errorf("failure report %q lacks the cause", core.reports[0].StatusJSON)
	}
}

func TestReconcilePanicIsRecoveredPerItem(t *testing.T) {
	core := &fakeCore{}
	rt := &fakeRuntime{panicOn: "boom"}
	w := newTestWorker(core, rt, nil)

	items := []coreclient.WorkItem{
		{ResourceUUID: "res-bad", Name: "boom", Generation: 1, SpecJSON: envelopeJSON(t, kitruntime.WorkloadSpec{Image: "a:1", Name: "boom"}, "", false)},
		{ResourceUUID: "res-good", Name: "ok", Generation: 1, SpecJSON: envelopeJSON(t, kitruntime.WorkloadSpec{Image: "a:1", Name: "ok"}, "", false)},
	}
	w.reconcileBatch(context.Background(), items, true)

	states := core.reportedStates()
	if got := states["res-bad"]; len(got) != 1 || got[0] != "failed" {
		t.Errorf("res-bad states = %v, want [failed]", got)
	}
	if got := states["res-good"]; len(got) != 1 || got[0] != "running" {
		t.Errorf("res-good states = %v, want [running] — the panic must not poison the batch", got)
	}
}

// --- static cache + offline supervision ---

func TestPersistSystemItemsCachesOnlySystemTier(t *testing.T) {
	cache := statecache.New(t.TempDir())
	core := &fakeCore{}
	rt := &fakeRuntime{}
	w := newTestWorker(core, rt, cache)

	sysSpec := kitruntime.WorkloadSpec{Image: "auth:1", Name: "auth"}
	userSpec := kitruntime.WorkloadSpec{Image: "blog:1", Name: "blog"}
	w.persistSystemItems([]coreclient.WorkItem{
		{ResourceUUID: "sys-1", Kind: "container", Name: "auth", Generation: 2, SpecJSON: envelopeJSON(t, sysSpec, "system", false)},
		{ResourceUUID: "usr-1", Kind: "container", Name: "blog", Generation: 1, SpecJSON: envelopeJSON(t, userSpec, "", false)},
	})

	items, err := cache.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(items) != 1 || items[0].ResourceUUID != "sys-1" {
		t.Fatalf("cached = %+v, want only sys-1 (non-system work must pause offline)", items)
	}
	if items[0].Generation != 2 {
		t.Errorf("generation = %d, want 2", items[0].Generation)
	}
}

func TestPersistSystemItemsMergesAndRemovesTeardowns(t *testing.T) {
	cache := statecache.New(t.TempDir())
	w := newTestWorker(&fakeCore{}, &fakeRuntime{}, cache)
	sysSpec := kitruntime.WorkloadSpec{Image: "auth:1", Name: "auth"}

	w.persistSystemItems([]coreclient.WorkItem{
		{ResourceUUID: "sys-1", Name: "auth", Generation: 1, SpecJSON: envelopeJSON(t, sysSpec, "system", false)},
	})
	// A later pull that does NOT mention sys-1 must not evict it (merge, not replace).
	w.persistSystemItems([]coreclient.WorkItem{
		{ResourceUUID: "sys-2", Name: "secret", Generation: 1, SpecJSON: envelopeJSON(t, sysSpec, "system", false)},
	})
	items, _ := cache.Load()
	if len(items) != 2 {
		t.Fatalf("cached = %d items, want 2 (quiet items must survive later pulls)", len(items))
	}
	// A teardown evicts — resurrecting a deleted service offline would undo the operator.
	w.persistSystemItems([]coreclient.WorkItem{
		{ResourceUUID: "sys-1", Name: "auth", Generation: 2, SpecJSON: `{"tier":"system","teardown":true}`},
	})
	items, _ = cache.Load()
	if len(items) != 1 || items[0].ResourceUUID != "sys-2" {
		t.Fatalf("cached = %+v, want only sys-2 after teardown", items)
	}
}

func TestOfflineTransitionAndRecovery(t *testing.T) {
	cache := statecache.New(t.TempDir())
	core := &fakeCore{}
	rt := &fakeRuntime{}
	w := New(core, rt, cache, Options{AgentUUID: "agent-1", OfflineThreshold: 3})

	sysSpec := kitruntime.WorkloadSpec{Image: "auth:1", Name: "auth"}
	userSpec := kitruntime.WorkloadSpec{Image: "blog:1", Name: "blog"}

	// A healthy pull caches the system item.
	core.pullItems = []coreclient.WorkItem{
		{ResourceUUID: "sys-1", Name: "auth", Generation: 1, SpecJSON: envelopeJSON(t, sysSpec, "system", false)},
		{ResourceUUID: "usr-1", Name: "blog", Generation: 1, SpecJSON: envelopeJSON(t, userSpec, "", false)},
	}
	w.pullTick(context.Background())
	if w.isOffline() {
		t.Fatal("offline after a successful pull")
	}

	// Pull failures below the threshold do not flip offline.
	core.pullErr = errors.New("core gone")
	w.pullTick(context.Background())
	w.pullTick(context.Background())
	if w.isOffline() {
		t.Fatal("offline before threshold")
	}

	// The threshold-th failure enters offline supervision: cached system
	// items reconcile again; user items do not.
	before := len(rt.ensuredNames())
	w.pullTick(context.Background())
	if !w.isOffline() {
		t.Fatal("not offline at threshold")
	}
	names := rt.ensuredNames()[before:]
	if len(names) != 1 || names[0] != "auth" {
		t.Fatalf("offline reconciled %v, want only the system item [auth]", names)
	}

	// Recovery: a successful pull flips back online and signals re-register.
	core.pullErr = nil
	core.pullItems = nil
	w.pullTick(context.Background())
	if w.isOffline() {
		t.Fatal("still offline after a successful pull")
	}
	select {
	case <-w.reRegister:
	default:
		t.Error("recovery did not request re-registration")
	}
}

func TestOfflineSupervisionPrimesCacheFromDisk(t *testing.T) {
	dir := t.TempDir()
	seed := statecache.New(dir)
	sysSpec := kitruntime.WorkloadSpec{Image: "auth:1", Name: "auth"}
	if err := seed.Save([]statecache.Item{{
		ResourceUUID: "sys-1", Kind: "container", Name: "auth",
		SpecJSON: envelopeJSON(t, sysSpec, "system", false), Generation: 4,
	}}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// Fresh worker (agent restarted straight into an outage).
	core := &fakeCore{pullErr: errors.New("core gone")}
	rt := &fakeRuntime{}
	w := New(core, rt, statecache.New(dir), Options{AgentUUID: "agent-1", OfflineThreshold: 1})

	w.pullTick(context.Background())
	if got := rt.ensuredNames(); len(got) != 1 || got[0] != "auth" {
		t.Fatalf("ensured = %v, want [auth] from the on-disk cache", got)
	}
	// Offline reconciles must not report — core is unreachable.
	if len(core.reports) != 0 {
		t.Errorf("reports = %+v, want none while offline", core.reports)
	}
}

// --- drift ---

func TestDriftReportsTransitions(t *testing.T) {
	core := &fakeCore{}
	rt := &fakeRuntime{statuses: map[string]kitruntime.WorkloadStatus{}}
	w := newTestWorker(core, rt, nil)

	// Converge once so the worker tracks res-1 → ctr-res-1 at generation 9.
	w.reconcileItem(context.Background(), coreclient.WorkItem{
		ResourceUUID: "res-1", Name: "app", Generation: 9,
		SpecJSON: envelopeJSON(t, kitruntime.WorkloadSpec{Image: "app:1", Name: "app"}, "", false),
	}, true)
	core.mu.Lock()
	core.reports = nil
	core.mu.Unlock()

	rt.mu.Lock()
	rt.listOut = []kitruntime.WorkloadStatus{{ID: "ctr-res-1"}}
	rt.statuses["ctr-res-1"] = kitruntime.WorkloadStatus{ID: "ctr-res-1", State: "exited", Running: false, ExitCode: 137, Health: kitruntime.HealthNone}
	rt.mu.Unlock()

	// First drift tick: running → exited is a transition and gets reported
	// at the last-known generation.
	w.driftTick(context.Background())
	if len(core.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(core.reports))
	}
	r := core.reports[0]
	if r.State != "exited" || r.ResourceUUID != "res-1" || r.ObservedGeneration != 9 {
		t.Errorf("report = %+v, want exited/res-1/gen 9", r)
	}
	if !strings.Contains(r.StatusJSON, "137") {
		t.Errorf("status JSON %q lacks the exit code", r.StatusJSON)
	}

	// Second tick with no change: no duplicate report.
	w.driftTick(context.Background())
	if len(core.reports) != 1 {
		t.Errorf("reports = %d after unchanged tick, want still 1", len(core.reports))
	}
}

func TestDriftReportsUnhealthy(t *testing.T) {
	core := &fakeCore{}
	rt := &fakeRuntime{statuses: map[string]kitruntime.WorkloadStatus{}}
	w := newTestWorker(core, rt, nil)

	w.reconcileItem(context.Background(), coreclient.WorkItem{
		ResourceUUID: "res-1", Name: "app", Generation: 2,
		SpecJSON: envelopeJSON(t, kitruntime.WorkloadSpec{Image: "app:1", Name: "app"}, "", false),
	}, true)
	core.mu.Lock()
	core.reports = nil
	core.mu.Unlock()

	rt.mu.Lock()
	rt.listOut = []kitruntime.WorkloadStatus{{ID: "ctr-res-1"}}
	rt.statuses["ctr-res-1"] = kitruntime.WorkloadStatus{ID: "ctr-res-1", State: "running", Running: true, Health: kitruntime.HealthUnhealthy}
	rt.mu.Unlock()

	w.driftTick(context.Background())
	if len(core.reports) != 1 || core.reports[0].State != "unhealthy" {
		t.Fatalf("reports = %+v, want one unhealthy", core.reports)
	}
}

// --- registration + heartbeat loops ---

func TestRegistrationRetriesUntilSuccess(t *testing.T) {
	core := &fakeCore{registerErrs: 2}
	w := newTestWorker(core, &fakeRuntime{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w.registerWithBackoff(ctx)

	core.mu.Lock()
	defer core.mu.Unlock()
	if core.registers != 3 {
		t.Errorf("registers = %d, want 3 (two failures then success)", core.registers)
	}
}

func TestHeartbeatLoopTicksIndependently(t *testing.T) {
	core := &fakeCore{}
	w := New(core, &fakeRuntime{}, nil, Options{AgentUUID: "agent-1", HeartbeatInterval: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.heartbeatLoop(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		core.mu.Lock()
		n := core.heartbeats
		core.mu.Unlock()
		if n >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("heartbeat loop did not tick 3 times within 2s")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}
