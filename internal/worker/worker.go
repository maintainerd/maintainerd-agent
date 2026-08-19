// Package worker is the agent's execution loop — the pull-model middle of the
// control loop. Each tick it pulls work from Core, executes it against the
// runtime (maintainerd-docker), and reports observed status back to Core.
package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	corev1 "github.com/maintainerd/core/gen/maintainerd/core/v1"
	runtimev1 "github.com/maintainerd/docker/gen/maintainerd/runtime/v1"

	"github.com/maintainerd/agent/internal/coreclient"
	"github.com/maintainerd/agent/internal/runtimeclient"
)

// Worker owns the pull → execute → report loop.
type Worker struct {
	core      *coreclient.Client // nil when CORE_ADDR is unset (runtime-only mode)
	rt        *runtimeclient.Client
	agentUUID string
	interval  time.Duration
	maxItems  int32
}

func New(core *coreclient.Client, rt *runtimeclient.Client, agentUUID string, interval time.Duration) *Worker {
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

	// Keepalive (best-effort — needs the agent to exist in Core).
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
func (w *Worker) reconcile(ctx context.Context, it *corev1.WorkItem) {
	var spec containerSpec
	if err := json.Unmarshal([]byte(it.GetSpecJson()), &spec); err != nil {
		w.report(ctx, it, "failed", map[string]any{"error": "invalid spec: " + err.Error()})
		return
	}
	name := spec.Name
	if name == "" {
		name = it.GetName()
	}
	slog.Info("reconciling resource", "resource", it.GetResourceUuid(), "kind", it.GetKind(), "image", spec.Image, "name", name)

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
	handle, err := w.rt.Run(runCtx, &runtimev1.WorkloadSpec{
		Image:  spec.Image,
		Name:   name,
		Cmd:    spec.Cmd,
		Env:    spec.Env,
		Labels: map[string]string{"maintainerd.resource": it.GetResourceUuid()},
	})
	if err != nil {
		w.report(ctx, it, "failed", map[string]any{"error": "run: " + err.Error()})
		return
	}
	w.report(ctx, it, "running", map[string]any{"container_id": handle.GetId()})
}

// report writes observed status back to Core, advancing observed_generation so
// an in-sync (or failed) resource is not pulled again.
func (w *Worker) report(ctx context.Context, it *corev1.WorkItem, state string, status map[string]any) {
	payload, _ := json.Marshal(status)
	repCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := w.core.ReportStatus(repCtx, w.agentUUID, []*corev1.StatusReport{{
		ResourceUuid:       it.GetResourceUuid(),
		State:              state,
		StatusJson:         string(payload),
		ObservedGeneration: it.GetGeneration(),
	}}); err != nil {
		slog.Warn("report status failed", "resource", it.GetResourceUuid(), "error", err.Error())
		return
	}
	slog.Info("reported status", "resource", it.GetResourceUuid(), "state", state)
}
