package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

// MARK: Login()
// ユーザー名とパスワードを検証し、セッショントークンを発行する。
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	// クライアントから送られた資格情報をパースする
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		logger.Logf("Client", "Auth", "ログインリクエストのパース失敗: %v", err)
		http.Error(w, "リクエストが不正です", http.StatusBadRequest)
		return
	}

	cfg := s.Config.Get()
	user, ok := cfg.Users[creds.Username]
	// 登録済みユーザーか、およびパスワードが一致するかを検証する
	if !ok || user.Password != creds.Password {
		logger.Logf("Client", "Auth", "認証失敗: user=%s", creds.Username)
		http.Error(w, "認証に失敗しました", http.StatusUnauthorized)
		return
	}

	// セッション維持のための推測困難なトークンを生成する
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		logger.Error("Auth", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(tokenBytes)

	// 生成したトークンをサーバー側のメモリに保持し、以降のリクエストで照合可能にする
	s.WebSessionMu.Lock()
	s.WebSessions[token] = creds.Username
	s.WebSessionMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"token": token}); err != nil {
		logger.Error("Auth", err)
	}
}

// MARK: Auth()
// 認証が必要なエンドポイント用のミドルウェア。
func (s *Server) Auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ヘッダーまたはクエリパラメータから認証トークンを抽出する
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		// 有効なセッションが存在するかチェックする
		s.WebSessionMu.RLock()
		username, ok := s.WebSessions[token]
		s.WebSessionMu.RUnlock()

		if !ok {
			http.Error(w, "認証が必要です", http.StatusUnauthorized)
			return
		}

		// コンテナ操作のリクエストである場合、ユーザーに対象コンテナの操作権限があるか検証する
		if id := r.URL.Query().Get("id"); id != "" {
			cfg := s.Config.Get()
			user := cfg.Users[username]

			// Docker上の実名（コンテナ名）を取得して照合を行う（ID直接指定にも対応）
			inspect, err := docker.Client.ContainerInspect(r.Context(), id)
			var realName string
			if err == nil {
				realName = inspect.Name[1:]
			} else {
				// 未作成コンテナ（missing）の場合は、リクエスト時のIDをサーバー名とみなす
				realName = id
			}

			// 認可チェック。指定されたコンテナ名が許可リストに含まれているか確認する。
			if !s.isAllowed(user, realName) {
				logger.Logf("Client", "Auth", "操作拒否: user=%s, target=%s", username, realName)
				http.Error(w, "このコンテナの操作権限がありません", http.StatusForbidden)
				return
			}
		}

		// 認証・認可がパスしたため、次のプロセッサに制御を委譲する
		next(w, r)
	}
}
