// Package server exposes a thin HTTP liveness/readiness surface for the agent.
// The serve loop and JSON helper come from maintainerd-kit; only the agent's
// own routes (the runtime readiness probe) live here.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	kitserver "github.com/maintainerd/kit/server"

	sdkruntime "github.com/maintainerd/agent/internal/runtimeclient"
)

// Server adapts the agent's health to HTTP.
type Server struct {
	rt *sdkruntime.Client
}

func New(rt *sdkruntime.Client) *Server { return &Server{rt: rt} }

// Router builds the HTTP routes.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)

	// Liveness: the agent process is up.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		kitserver.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness: the agent's runtime (docker) is reachable.
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.rt.Ping(r.Context()); err != nil {
			kitserver.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not-ready", "error": err.Error()})
			return
		}
		kitserver.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return r
}
