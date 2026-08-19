// Package worker is the agent's execution loop. In the full system it pulls work
// from Core (core.v1) and executes it against the runtime (maintainerd-docker).
// For the base it runs the loop skeleton; Core wiring lands when core.v1 exists.
package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/maintainerd/agent/internal/runtimeclient"
)

// Worker owns the pull-execute-report loop.
type Worker struct {
	rt       *runtimeclient.Client
	interval time.Duration
}

func New(rt *runtimeclient.Client, interval time.Duration) *Worker {
	return &Worker{rt: rt, interval: interval}
}

// Run drives the loop until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	slog.Info("work loop started", "interval", w.interval.String())
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

// tick is one iteration. Today it only confirms the runtime is reachable; once
// core.v1 exists it will: (1) pull work from Core, (2) execute via w.rt, and
// (3) report observed status back to Core.
func (w *Worker) tick(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := w.rt.Ping(pingCtx); err != nil {
		slog.Warn("runtime unreachable this tick", "error", err.Error())
		return
	}
	// TODO(core.v1): pull work from Core and execute it via the runtime client.
	slog.Debug("tick: runtime healthy, awaiting core.v1 work protocol")
}
