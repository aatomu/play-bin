package api

import (
	"encoding/json"
	"net/http"

	ctypes "github.com/docker/docker/api/types/container"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/docker"
)

// MARK: ListContainers
func (s *Server) ListContainers(w http.ResponseWriter, r *http.Request) {
	containers, err := docker.Client.ContainerList(r.Context(), ctypes.ListOptions{All: true})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	s.WebSessionMu.RLock()
	username := s.WebSessions[token]
	s.WebSessionMu.RUnlock()

	cfg := s.Config.Get()
	user := cfg.Users[username]

	filtered := []ctypes.Summary{}
	for _, c := range containers {
		isAllowed := false
		if len(c.Names) > 0 {
			name := c.Names[0][1:]
			for _, pattern := range user.Controllable {
				if pattern == "*" || pattern == name {
					isAllowed = true
					break
				}
			}
		}
		if isAllowed {
			filtered = append(filtered, c)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(filtered)
}

// MARK: InspectContainer
func (s *Server) InspectContainer(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	inspect, err := docker.Client.ContainerInspect(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(inspect)
}

// MARK: Action
func (s *Server) Action(action container.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		err := s.ContainerManager.ExecuteAction(r.Context(), id, action)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
