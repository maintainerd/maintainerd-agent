// Package worker is the agent's execution loop — the pull-model middle of the
// control loop. Each tick it pulls work from Core, executes it against the
// runtime, and reports observed status back — all via the maintainerd SDK.
package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	sdkcore "github.com/maintainerd/agent/internal/coreclient"
	sdkruntime "github.com/maintainerd/agent/internal/runtimeclient"
)

// Worker owns the pull → execute → report loop.
type Worker struct {
	core      *sdkcore.Client // nil when CORE_ADDR is unset (runtime-only mode)
	rt        *sdkruntime.Client
	agentUUID string
	interval  time.Duration
	maxItems  int32
}

func New(core *sdkcore.Client, rt *sdkruntime.Client, agentUUID string, interval time.Duration) *Worker {
	return &Worker{core: core, rt: rt, agentUUID: agentUUID, interval: interval, maxItems: 10}
}

// Run drives the loop until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	slog.Info("work loop started", "interval", w.interval.String(), "control_plane", w.core != nil)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("work loop stopped")
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := w.rt.Ping(pingCtx)
	cancel()
	if err != nil {
		slog.Warn("runtime unreachable this tick", "error", err.Error())
		return
	}
	if w.core == nil {
		return // runtime-only mode: nothing to pull
	}

	hbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = w.core.Heartbeat(hbCtx, w.agentUUID)
	cancel()

	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	items, err := w.core.PullWork(pullCtx, w.agentUUID, w.maxItems)
	cancel()
	if err != nil {
		slog.Warn("pull work failed", "error", err.Error())
		return
	}
	for _, it := range items {
		w.reconcile(ctx, it)
	}
}

// containerSpec is the shape the agent expects inside a resource's spec JSON for
// kind "container".
type containerSpec struct {
	Image string            `json:"image"`
	Name  string            `json:"name"`
	Cmd   []string          `json:"cmd"`
	Env   map[string]string `json:"env"`
}

// reconcile drives one work item toward its desired spec: pull the image, run
// the container, and report the observed result back to Core.
func (w *Worker) reconcile(ctx context.Context, it sdkcore.WorkItem) {
	var spec containerSpec
	if err := json.Unmarshal([]byte(it.SpecJSON), &spec); err != nil {
		w.report(ctx, it, "failed", map[string]any{"error": "invalid spec: " + err.Error()})
		return
	}
	name := spec.Name
	if name == "" {
		name = it.Name
	}
	slog.Info("reconciling resource", "resource", it.ResourceUUID, "kind", it.Kind, "image", spec.Image, "name", name)

	if spec.Image == "" {
		w.report(ctx, it, "failed", map[string]any{"error": "spec.image is required"})
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if err := w.rt.Pull(runCtx, spec.Image); err != nil {
		w.report(ctx, it, "failed", map[string]any{"error": "pull: " + err.Error()})
		return
	}
	id, err := w.rt.Run(runCtx, sdkruntime.Spec{
		Image:  spec.Image,
		Name:   name,
		Cmd:    spec.Cmd,
		Env:    spec.Env,
		Labels: map[string]string{"maintainerd.resource": it.ResourceUUID},
	})
	if err != nil {
		w.report(ctx, it, "failed", map[string]any{"error": "run: " + err.Error()})
		return
	}
	w.report(ctx, it, "running", map[string]any{"container_id": id})
}

// report writes observed status back to Core, advancing observed_generation so
// an in-sync (or failed) resource is not pulled again.
func (w *Worker) report(ctx context.Context, it sdkcore.WorkItem, state string, status map[string]any) {
	payload, _ := json.Marshal(status)
	repCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := w.core.ReportStatus(repCtx, w.agentUUID, []sdkcore.StatusReport{{
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
