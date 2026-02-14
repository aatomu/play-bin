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

	// クライアントから送られた資格情報をパースする。
	// フォーマット不正は即座にクライアント側の誤り（Client）として却下する。
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		logger.Logf("Client", "Auth", "ログインリクエストのパース失敗: %v", err)
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	cfg := s.Config.Get()
	user, ok := cfg.Users[creds.Username]

	// 登録済みユーザーか、およびパスワードが一致するかを検証する。
	// 認証の失敗はセキュリティ監視のため、対象ユーザー名を添えて記録する。
	if !ok || user.Password != creds.Password {
		logger.Logf("Client", "Auth", "認証失敗: user=%s", creds.Username)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// セッション維持のための、十分なエントロピーを持つ推測困難なトークンを生成する。
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		// 乱数生成の失敗はOSレベルの重大な障害（Internal）として扱う。
		logger.Logf("Internal", "Auth", "トークン生成用乱数取得失敗: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(tokenBytes)

	// 生成したトークンをサーバー側のメモリに保持し、以降のリクエストで照合可能にする。
	s.WebSessionMu.Lock()
	s.WebSessions[token] = creds.Username
	s.WebSessionMu.Unlock()

	logger.Logf("Internal", "Auth", "ログイン成功: user=%s", creds.Username)

	// 成功応答としてトークンをクライアントに返却する。
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"token": token}); err != nil {
		logger.Logf("Internal", "Auth", "JSONエンコード失敗: %v", err)
	}
}

// MARK: Auth()
// 認証が必要なエンドポイント用のミドルウェア。
func (s *Server) Auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ヘッダーまたはクエリパラメータから認証トークンを抽出する。
		// WS接続時などはヘッダーが使えないため、クエリパラメータもサポートしている。
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		// 有効なセッションが存在するかチェックする。
		s.WebSessionMu.RLock()
		username, ok := s.WebSessions[token]
		s.WebSessionMu.RUnlock()

		if !ok {
			// 未認証またはトークン期限切れ（メモリ上の抹消）の場合は401を返す。
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}

		// コンテナ操作のリクエストである場合、ユーザーに対象コンテナの操作権限があるか検証する。
		if serverName := r.URL.Query().Get("id"); serverName != "" {
			cfg := s.Config.Get()
			user := cfg.Users[username]

			// Docker上の実名（コンテナ名）を取得して照合を行う（ID直接指定にも対応）。
			inspect, err := docker.Client.ContainerInspect(r.Context(), serverName)
			var realName string
			if err == nil {
				realName = inspect.Name[1:]
			} else {
				// 未作成コンテナ（missing）の場合は、リクエスト時のIDをサーバー名とみなす。
				realName = serverName
			}

			// 認可チェック。指定されたコンテナ名が許可リストに含まれているか確認する。
			if !user.HasPermission(realName, "read") {
				// 権限外の操作試行は重要な監視対象（Client）として記録する。
				logger.Logf("Client", "Auth", "操作拒否: user=%s, target=%s", username, realName)
				http.Error(w, "Operation not allowed for this container", http.StatusForbidden)
				return
			}
		}

		// 認証・認可がパスしたため、次のプロセッサに制御を委譲する。
		next(w, r)
	}
}
