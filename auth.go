package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
)

// MARK: handleLogin()
func handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// ユーザー確認
	cfg := getConfig()
	user, ok := cfg.Users[creds.Username]
	if !ok || user.Password != creds.Password {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// トークン生成
	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	// セッション保存 (ユーザーIDを紐付け)
	sessionLock.Lock()
	sessions[token] = creds.Username
	sessionLock.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// MARK: auth()
func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// セッションチェック
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		sessionLock.RLock()
		username, ok := sessions[token]
		sessionLock.RUnlock()

		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// 権限チェック
		containerID := r.URL.Query().Get("id")
		if containerID != "" {
			cfg := getConfig()
			user, exists := cfg.Users[username]
			if !exists {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			// Dockerからコンテナの実際の名前を取得
			inspect, err := cli.ContainerInspect(r.Context(), containerID)
			if err != nil {
				http.Error(w, "Container Not Found", http.StatusNotFound)
				return
			}
			// 先頭の "/" を消す
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
