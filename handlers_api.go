package main

import (
	"encoding/json"
	"net/http"

	"github.com/docker/docker/api/types/container"
)

func handleContainerList(w http.ResponseWriter, r *http.Request) {
	containers, err := cli.ContainerList(r.Context(), container.ListOptions{All: true})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	sessionLock.RLock()
	username := sessions[token]
	sessionLock.RUnlock()

	cfg := getConfig()
	user := cfg.Users[username]

	// フィルタリング
	filtered := []container.Summary{}
	for _, c := range containers {
		isAllowed := false
		// 最初の1つをチェック
		if len(c.Names) > 0 {
			// 先頭の "/" を消す
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

func handleContainerInspect(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	inspect, err := cli.ContainerInspect(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(inspect)
}
