// Package worker is the agent's convergence engine — the pull-model middle of
// the control loop. It pulls desired state from Core, converges container
// workloads through the compiled-in runtime driver, observes drift, and
// reports observed status back. Four independent loops share one Worker:
//
//   - heartbeat: its own goroutine + ticker, NEVER blocked by reconciles — a
//     10-minute image pull must not make Core think the host is down.
//   - pull → reconcile: pulls work each tick and converges items on a bounded
//     pool, so one slow item cannot serialize the batch.
//   - drift: periodically re-observes owned workloads and reports state
//     transitions (exited, unhealthy) — what keeps "running" honest.
//   - registration: retried with backoff until Core accepts it, and repeated
//     after every offline→online transition.
//
// When Core is unreachable past a threshold the worker enters offline
// supervision: it keeps converging the system-tier items persisted in the
// state cache (auth, secret, core itself must never die because the thing
// that schedules them is away), while ordinary work correctly pauses.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	kitruntime "github.com/maintainerd/kit/runtime"

	"github.com/maintainerd/agent/internal/coreclient"
	"github.com/maintainerd/agent/internal/statecache"
)

// tierSystem marks work items the agent must keep alive even when Core is
// unreachable (the platform's static tier).
const tierSystem = "system"

// CoreClient is the slice of the core client the worker uses; an interface so
// tests fake the control plane. *coreclient.Client satisfies it.
type CoreClient interface {
	Register(ctx context.Context, agentUUID, version string, capabilities []string) error
	Heartbeat(ctx context.Context, agentUUID string) error
	PullWork(ctx context.Context, agentUUID string, max int32) ([]coreclient.WorkItem, error)
	ReportStatus(ctx context.Context, agentUUID string, reports []coreclient.StatusReport) (int32, error)
}

// Options configures a Worker. Zero fields take the documented defaults.
type Options struct {
	AgentUUID            string
	Version              string
	PollInterval         time.Duration // default 5s
	HeartbeatInterval    time.Duration // default 10s
	DriftInterval        time.Duration // default 30s
	ReconcileConcurrency int           // default 4
	OfflineThreshold     int           // default 3
	MaxItems             int32         // default 10
}

func (o *Options) fillDefaults() {
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 10 * time.Second
	}
	if o.DriftInterval <= 0 {
		o.DriftInterval = 30 * time.Second
	}
	if o.ReconcileConcurrency <= 0 {
		o.ReconcileConcurrency = 4
	}
	if o.OfflineThreshold <= 0 {
		o.OfflineThreshold = 3
	}
	if o.MaxItems <= 0 {
		o.MaxItems = 10
	}
}

// tracked is what the worker remembers about a resource it converged — the
// container it materialized to and the generation it observed. The drift loop
// uses it to attribute engine-side containers back to resources and to report
// transitions at the last-known generation.
type tracked struct {
	containerID string
	generation  int64
	lastState   string
	lastHealth  kitruntime.HealthState
}

// Worker owns the pull → converge → report control loop.
type Worker struct {
	core  CoreClient // nil when CORE_ADDR is unset (runtime-only mode)
	rt    kitruntime.Runtime
	cache *statecache.Cache
	opts  Options

	mu                  sync.Mutex
	pullFailures        int  // consecutive PullWork failures
	offline             bool // true once pullFailures >= OfflineThreshold
	byResource          map[string]*tracked
	byContainer         map[string]string // container id → resource uuid
	reRegister          chan struct{}     // signaled on offline→online recovery
	cachedItems         []statecache.Item // in-memory mirror of the system cache
	cachePrimedFromDisk bool
}

// New builds a Worker. core may be nil (runtime-only mode: the agent serves
// health and keeps the driver warm, but has no desired state to converge).
func New(core CoreClient, rt kitruntime.Runtime, cache *statecache.Cache, opts Options) *Worker {
	opts.fillDefaults()
	return &Worker{
		core:        core,
		rt:          rt,
		cache:       cache,
		opts:        opts,
		byResource:  map[string]*tracked{},
		byContainer: map[string]string{},
		reRegister:  make(chan struct{}, 1),
	}
}

// Run drives all loops until ctx is cancelled. It always returns nil on
// cancellation — a stopping worker is a clean shutdown, not an error.
func (w *Worker) Run(ctx context.Context) error {
	slog.Info("worker started",
		"control_plane", w.core != nil,
		"poll_interval", w.opts.PollInterval.String(),
		"heartbeat_interval", w.opts.HeartbeatInterval.String(),
		"drift_interval", w.opts.DriftInterval.String(),
		"reconcile_concurrency", w.opts.ReconcileConcurrency,
	)
	if w.core == nil {
		// Reachable in development only: outside it, the bootstrap refuses to
		// start without CORE_ADDR (cmd/agentd requireControlPlane), because an
		// agent that converges nothing while reporting healthy never alerts.
		slog.Warn("CORE_ADDR not set — running runtime-only, no control plane (development only)")
		<-ctx.Done()
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); w.registrationLoop(ctx) }()
	go func() { defer wg.Done(); w.heartbeatLoop(ctx) }()
	go func() { defer wg.Done(); w.pullLoop(ctx) }()
	go func() { defer wg.Done(); w.driftLoop(ctx) }()
	wg.Wait()
	slog.Info("worker stopped")
	return nil
}

// --- registration ---

// registrationLoop registers with backoff until Core accepts, then re-registers
// after every reconnect. Fire-and-forget registration would leave an agent
// invisible to the fleet after one unlucky boot-time network blip.
func (w *Worker) registrationLoop(ctx context.Context) {
	for {
		w.registerWithBackoff(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.reRegister:
			slog.Info("control plane reconnected — re-registering")
		}
	}
}

func (w *Worker) registerWithBackoff(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		regCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := w.core.Register(regCtx, w.opts.AgentUUID, w.opts.Version, []string{"container"})
		cancel()
		if err == nil {
			slog.Info("registered with core", "agent_uuid", w.opts.AgentUUID)
			return
		}
		slog.Warn("register with core failed — retrying", "error", err.Error(), "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// --- heartbeat ---

// heartbeatLoop beats on its own ticker, decoupled from reconciles by design:
// liveness must reflect the host, not the length of the current work batch.
func (w *Worker) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := w.core.Heartbeat(hbCtx, w.opts.AgentUUID); err != nil {
				slog.Warn("heartbeat failed", "error", err.Error())
			}
			cancel()
		}
	}
}

// --- pull + reconcile ---

func (w *Worker) pullLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pullTick(ctx)
		}
	}
}

func (w *Worker) pullTick(ctx context.Context) {
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	items, err := w.core.PullWork(pullCtx, w.opts.AgentUUID, w.opts.MaxItems)
	cancel()
	if err != nil {
		w.onPullFailure(ctx, err)
		return
	}
	w.onPullSuccess(ctx, items)
}

func (w *Worker) onPullSuccess(ctx context.Context, items []coreclient.WorkItem) {
	w.mu.Lock()
	w.pullFailures = 0
	wasOffline := w.offline
	w.offline = false
	w.mu.Unlock()
	if wasOffline {
		slog.Info("control plane reachable again — resuming normal operation")
		select {
		case w.reRegister <- struct{}{}:
		default:
		}
	}

	w.persistSystemItems(items)
	w.reconcileBatch(ctx, items, true)
}

func (w *Worker) onPullFailure(ctx context.Context, err error) {
	w.mu.Lock()
	w.pullFailures++
	failures := w.pullFailures
	justWentOffline := !w.offline && failures >= w.opts.OfflineThreshold
	if justWentOffline {
		w.offline = true
	}
	offline := w.offline
	w.mu.Unlock()

	slog.Warn("pull work failed", "error", err.Error(), "consecutive_failures", failures)
	if justWentOffline {
		slog.Warn("control plane unreachable — entering offline supervision of the system tier",
			"threshold", w.opts.OfflineThreshold)
	}
	if offline {
		w.superviseOffline(ctx)
	}
}

// superviseOffline converges the cached system-tier items so platform
// services survive the outage. Reports are skipped — Core is unreachable, and
// the drift loop re-reports honestly once it is back.
func (w *Worker) superviseOffline(ctx context.Context) {
	items := w.loadCachedItems()
	if len(items) == 0 {
		return
	}
	work := make([]coreclient.WorkItem, 0, len(items))
	for _, it := range items {
		work = append(work, coreclient.WorkItem{
			ResourceUUID: it.ResourceUUID,
			Kind:         it.Kind,
			Name:         it.Name,
			SpecJSON:     it.SpecJSON,
			Generation:   it.Generation,
		})
	}
	w.reconcileBatch(ctx, work, false)
}

// loadCachedItems returns the in-memory mirror, priming it from disk once
// (the agent may have restarted straight into an outage).
func (w *Worker) loadCachedItems() []statecache.Item {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cachePrimedFromDisk && len(w.cachedItems) == 0 && w.cache != nil {
		w.cachePrimedFromDisk = true
		items, err := w.cache.Load()
		if err != nil {
			slog.Error("state cache unreadable — offline supervision degraded", "error", err.Error())
			return nil
		}
		w.cachedItems = items
	}
	return w.cachedItems
}

// persistSystemItems merges this pull's system-tier items into the cache and
// persists it atomically. Merge (not replace): PullWork returns only items
// needing reconciliation, so a quiet system service must not fall out of the
// cache just because it had nothing to do this tick. Teardowns are removed —
// resurrecting a deleted service offline would undo an operator's decision.
func (w *Worker) persistSystemItems(items []coreclient.WorkItem) {
	if w.cache == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.cachePrimedFromDisk {
		w.cachePrimedFromDisk = true
		if existing, err := w.cache.Load(); err == nil {
			w.cachedItems = existing
		} else {
			slog.Warn("state cache unreadable — rebuilding from live pulls", "error", err.Error())
		}
	}

	merged := make(map[string]statecache.Item, len(w.cachedItems)+len(items))
	for _, it := range w.cachedItems {
		merged[it.ResourceUUID] = it
	}
	changed := false
	for _, it := range items {
		env, err := parseEnvelope(it.SpecJSON)
		if err != nil {
			continue // invalid specs are reported by reconcile; never cached
		}
		if env.Tier != tierSystem {
			continue
		}
		if env.Teardown {
			if _, ok := merged[it.ResourceUUID]; ok {
				delete(merged, it.ResourceUUID)
				changed = true
			}
			continue
		}
		next := statecache.Item{
			ResourceUUID: it.ResourceUUID,
			Kind:         it.Kind,
			Name:         it.Name,
			SpecJSON:     it.SpecJSON,
			Generation:   it.Generation,
		}
		if prev, ok := merged[it.ResourceUUID]; !ok || prev != next {
			merged[it.ResourceUUID] = next
			changed = true
		}
	}
	if !changed {
		return
	}
	list := make([]statecache.Item, 0, len(merged))
	for _, it := range merged {
		list = append(list, it)
	}
	w.cachedItems = list
	if err := w.cache.Save(list); err != nil {
		slog.Error("persist system-tier cache failed", "error", err.Error())
	}
}

// reconcileBatch converges items on a bounded pool. report=false during
// offline supervision (Core is unreachable).
func (w *Worker) reconcileBatch(ctx context.Context, items []coreclient.WorkItem, report bool) {
	if len(items) == 0 {
		return
	}
	sem := make(chan struct{}, w.opts.ReconcileConcurrency)
	var wg sync.WaitGroup
	for _, it := range items {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(it coreclient.WorkItem) {
			defer wg.Done()
			defer func() { <-sem }()
			w.reconcileItem(ctx, it, report)
		}(it)
	}
	wg.Wait()
}

// reconcileItem drives one work item toward its desired state. Panics are
// recovered per item so one poisoned spec cannot take the whole agent down —
// the failure is reported as that item's state instead.
func (w *Worker) reconcileItem(ctx context.Context, it coreclient.WorkItem, report bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic while reconciling — reported as failed", "resource", it.ResourceUUID, "panic", fmt.Sprint(r))
			if report {
				w.report(ctx, it, "failed", map[string]any{"error": fmt.Sprintf("panic: %v", r)})
			}
		}
	}()

	env, err := parseEnvelope(it.SpecJSON)
	if err != nil {
		// Never guess at a malformed desired state: report and stop.
		if report {
			w.report(ctx, it, "failed", map[string]any{"error": "invalid spec: " + err.Error()})
		}
		return
	}

	// Per-item budget generous enough for a large image pull, bounded so a
	// wedged engine call cannot pin a pool slot forever.
	itemCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if env.Teardown {
		w.teardown(itemCtx, it, env, report)
		return
	}

	spec := env.Workload
	if spec.Name == "" {
		spec.Name = it.Name
	}
	// The worker owns the ownership label — the driver requires it, and
	// setting it here (not trusting the spec) pins the container to the
	// resource Core actually scheduled.
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[kitruntime.LabelResource] = it.ResourceUUID

	if err := spec.Validate(); err != nil {
		if report {
			w.report(ctx, it, "failed", map[string]any{"error": "invalid spec: " + err.Error()})
		}
		return
	}

	st, changed, err := w.rt.Ensure(itemCtx, spec)
	if err != nil {
		slog.Warn("ensure failed", "resource", it.ResourceUUID, "error", err.Error())
		if report {
			w.report(ctx, it, "failed", map[string]any{"error": err.Error()})
		}
		return
	}
	w.track(it.ResourceUUID, st, it.Generation)
	slog.Info("reconciled resource", "resource", it.ResourceUUID, "container", st.ID, "changed", changed)
	if report {
		w.report(ctx, it, "running", map[string]any{
			"container_id": st.ID,
			"health":       string(st.Health),
			"changed":      changed,
		})
	}
}

func (w *Worker) teardown(ctx context.Context, it coreclient.WorkItem, env envelope, report bool) {
	workloads, err := w.rt.List(ctx, map[string]string{kitruntime.LabelResource: it.ResourceUUID})
	if err != nil {
		if report {
			w.report(ctx, it, "failed", map[string]any{"error": "teardown list: " + err.Error()})
		}
		return
	}
	for _, wl := range workloads {
		if err := w.rt.Remove(ctx, wl.ID, env.Workload.RemoveVolumesOnTeardown); err != nil {
			if report {
				w.report(ctx, it, "failed", map[string]any{"error": "teardown remove: " + err.Error()})
			}
			return
		}
	}
	w.untrack(it.ResourceUUID)
	slog.Info("tore down resource", "resource", it.ResourceUUID, "removed", len(workloads))
	if report {
		w.report(ctx, it, "removed", map[string]any{"removed": len(workloads)})
	}
}

// --- drift observation ---

// driftLoop re-observes owned workloads and reports transitions. Creation
// reports say "running" once; without this loop nothing would ever tell Core
// about the crash five minutes later.
func (w *Worker) driftLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.DriftInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.driftTick(ctx)
		}
	}
}

func (w *Worker) driftTick(ctx context.Context) {
	if w.isOffline() {
		return // nothing to report to; offline supervision keeps things alive
	}
	// Empty label value = presence match: every workload this agent owns,
	// regardless of resource — and never anything else on the host.
	workloads, err := w.rt.List(ctx, map[string]string{kitruntime.LabelResource: ""})
	if err != nil {
		slog.Warn("drift list failed", "error", err.Error())
		return
	}
	for _, wl := range workloads {
		resource, gen, ok := w.resourceForContainer(wl.ID)
		if !ok {
			continue // not attributable (e.g. pre-restart container); the next reconcile re-adopts it
		}
		st, err := w.rt.Status(ctx, wl.ID)
		if err != nil {
			slog.Warn("drift status failed", "container", wl.ID, "error", err.Error())
			continue
		}
		if !w.updateObserved(resource, st) {
			continue // no transition
		}
		state := "running"
		if !st.Running {
			state = "exited"
		} else if st.Health == kitruntime.HealthUnhealthy {
			state = "unhealthy"
		}
		slog.Info("drift detected", "resource", resource, "state", state, "exit_code", st.ExitCode)
		w.report(ctx, coreclient.WorkItem{ResourceUUID: resource, Generation: gen}, state, map[string]any{
			"container_id": st.ID,
			"health":       string(st.Health),
			"exit_code":    st.ExitCode,
			"error":        st.Error,
		})
	}
}

// --- tracking helpers ---

func (w *Worker) track(resource string, st kitruntime.WorkloadStatus, generation int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if prev, ok := w.byResource[resource]; ok && prev.containerID != st.ID {
		delete(w.byContainer, prev.containerID)
	}
	state := "running"
	if !st.Running {
		state = st.State
	}
	w.byResource[resource] = &tracked{containerID: st.ID, generation: generation, lastState: state, lastHealth: st.Health}
	w.byContainer[st.ID] = resource
}

func (w *Worker) untrack(resource string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if prev, ok := w.byResource[resource]; ok {
		delete(w.byContainer, prev.containerID)
	}
	delete(w.byResource, resource)
}

func (w *Worker) resourceForContainer(containerID string) (string, int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	resource, ok := w.byContainer[containerID]
	if !ok {
		return "", 0, false
	}
	return resource, w.byResource[resource].generation, true
}

// updateObserved records the newly observed state and reports whether it is a
// transition worth telling Core about.
func (w *Worker) updateObserved(resource string, st kitruntime.WorkloadStatus) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	tr, ok := w.byResource[resource]
	if !ok {
		return false
	}
	state := st.State
	if state == "" && st.Running {
		state = "running"
	}
	changed := tr.lastState != state || tr.lastHealth != st.Health
	tr.lastState = state
	tr.lastHealth = st.Health
	return changed
}

func (w *Worker) isOffline() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offline
}

// report writes observed status back to Core, advancing observed_generation
// so an in-sync (or failed) resource is not pulled again.
func (w *Worker) report(ctx context.Context, it coreclient.WorkItem, state string, status map[string]any) {
	payload, _ := json.Marshal(status)
	repCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := w.core.ReportStatus(repCtx, w.opts.AgentUUID, []coreclient.StatusReport{{
		ResourceUUID:       it.ResourceUUID,
		State:              state,
		StatusJSON:         string(payload),
		ObservedGeneration: it.Generation,
	}}); err != nil {
		slog.Warn("report status failed", "resource", it.ResourceUUID, "error", err.Error())
		return
	}
	slog.Info("reported status", "resource", it.ResourceUUID, "state", state)
}
