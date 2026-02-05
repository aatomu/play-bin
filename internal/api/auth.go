package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"

	"github.com/play-bin/internal/docker"
)

// MARK: Login()
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	cfg := s.Config.Get()
	user, ok := cfg.Users[creds.Username]
	if !ok || user.Password != creds.Password {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	s.WebSessionMu.Lock()
	s.WebSessions[token] = creds.Username
	s.WebSessionMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// MARK: Auth()
func (s *Server) Auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		s.WebSessionMu.RLock()
		username, ok := s.WebSessions[token]
		s.WebSessionMu.RUnlock()

		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		containerID := r.URL.Query().Get("id")
		if containerID != "" {
			cfg := s.Config.Get()
			user, exists := cfg.Users[username]
			if !exists {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			inspect, err := docker.Client.ContainerInspect(r.Context(), containerID)
			if err != nil {
				http.Error(w, "Container Not Found", http.StatusNotFound)
				return
			}
			realName := inspect.Name[1:]

			allowed := false
			for _, pattern := range user.Controllable {
				if pattern == "*" || pattern == realName {
					allowed = true
					break
				}
			}

			if !allowed {
				log.Printf("Permission Denied: user=%s, target_name=%s", username, realName)
				http.Error(w, "Forbidden: No permission for this container name", http.StatusForbidden)
				return
			}
		}

		next(w, r)
	}
}
