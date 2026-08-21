// Package server exposes a thin HTTP liveness/readiness surface for the agent.
// The serve loop and JSON helper come from maintainerd-kit; only the agent's
// own routes (the runtime readiness probe) live here.
//
// /healthz and /readyz are deliberately UNAUTHENTICATED — the one exception to
// the agent's guarded-surface rule. Probes run before any credential exists
// (kubelet, load balancers, systemd watchdogs), and these endpoints return
// only a status word and, at worst, a runtime reachability error string —
// no inventory, no workload names, no configuration. Everything with actual
// information content lives on the gRPC surface behind token verification.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	kitruntime "github.com/maintainerd/kit/runtime"
	kitserver "github.com/maintainerd/kit/server"
)

// Server adapts the agent's health to HTTP.
type Server struct {
	rt kitruntime.Runtime
}

func New(rt kitruntime.Runtime) *Server { return &Server{rt: rt} }

// Router builds the HTTP routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)

	// Liveness: the agent process is up.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		kitserver.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: the agent's container engine is reachable.
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.rt.Ping(r.Context()); err != nil {
			kitserver.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not-ready", "error": err.Error()})
			return
		}
		kitserver.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return r
}
