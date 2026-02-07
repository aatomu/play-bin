package api

import (
	"encoding/json"
	"io"
	"net/http"

	ctypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

// ContainerListItem はリスト表示用のコンテナ情報を表す。
type ContainerListItem struct {
	ID    string   `json:"id"`
	Names []string `json:"names"`
	State string   `json:"state"` // running, stopped, missing
}

// MARK: ListContainers()
// 管理対象および実在するコンテナのリストを返す。
func (s *Server) ListContainers(w http.ResponseWriter, r *http.Request) {
	// 現在のDocker上の全コンテナと管理対象設定を突き合わせるため、まずDockerから情報を取得する。
	containers, err := docker.Client.ContainerList(r.Context(), ctypes.ListOptions{All: true})
	if err != nil {
		// Dockerデーモンとの通信失敗はサーバー内部の問題としてログに記録する。
		logger.Logf("Internal", "API", "コンテナリストの取得に失敗: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// 突き合わせの効率化のため、Docker上のコンテナを名前をキーとしたマップに整理する。
	dockerMap := make(map[string]ctypes.Summary)
	for _, c := range containers {
		for _, name := range c.Names {
			// 先頭の '/' を除去して比較しやすくする。
			dockerMap[name[1:]] = c
		}
	}

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	// ユーザーごとの権限に基づいたフィルタリングを行うため、セッションからユーザー情報を特定する。
	s.WebSessionMu.RLock()
	username := s.WebSessions[token]
	s.WebSessionMu.RUnlock()

	cfg := s.Config.Get()
	user := cfg.Users[username]

	var result []ContainerListItem
	processedDockerNames := make(map[string]bool)

	// 1. 設定ファイルにあるサーバーを優先的に処理する。
	// これにより、Docker上にまだコンテナが作成されていない場合でも「missing」としてリストに表示できる。
	for serverName := range cfg.Servers {
		// 権限がないサーバーはリスト自体に表示させないことで、セキュリティと使い勝手を両立する。
		if !s.isAllowed(user, serverName) {
			continue
		}

		item := ContainerListItem{
			ID:    serverName,
			Names: []string{"/" + serverName},
		}

		if c, exists := dockerMap[serverName]; exists {
			item.State = c.State
			processedDockerNames[serverName] = true
		} else {
			item.State = "missing"
		}
		result = append(result, item)
	}

	// 2. 設定ファイルにはないが、Docker上に存在する（かつ権限のある）コンテナを追加する。
	// 管理外のコンテナであっても、権限さえあれば操作を可能にするための柔軟な設計に基づいている。
	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		name := c.Names[0][1:]
		if processedDockerNames[name] || !s.isAllowed(user, name) {
			continue
		}

		result = append(result, ContainerListItem{
			ID:    c.ID,
			Names: c.Names,
			State: c.State,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		// JSON変換の失敗はプログラムの不備（Internal）として扱う。
		logger.Logf("Internal", "API", "JSONエンコード失敗: %v", err)
	}
}

// MARK: isAllowed()
// ユーザーが特定のコンテナ名に対する操作権限を持っているか検証する。
func (s *Server) isAllowed(user config.UserConfig, name string) bool {
	// 設定ミスなどで許可リストが未定義の場合は、安全を優先して拒否（Whitelist方式）する。
	if user.Controllable == nil {
		return false
	}
	for _, pattern := range user.Controllable {
		// ワイルドカード指定または名前が完全一致する場合に許可する。
		if pattern == "*" || pattern == name {
			return true
		}
	}
	return false
}

// MARK: InspectContainer()
// コンテナの詳細情報を取得する。
func (s *Server) InspectContainer(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	// 詳細情報を取得し、フロントエンドでの詳細表示（スペックやネットワーク設定など）に利用する。
	inspect, err := docker.Client.ContainerInspect(r.Context(), id)
	if err != nil {
		// コンテナが見つからない原因はクライアントからの無効な指定（Client）として扱う。
		logger.Logf("Client", "API", "コンテナ %s の詳細取得失敗: %v", id, err)
		http.Error(w, "Container Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(inspect); err != nil {
		logger.Logf("Internal", "API", "JSONエンコード失敗: %v", err)
	}
}

// MARK: Action()
// コンテナに対して操作（起動・停止など）を実行するためのハンドラーを生成する。
func (s *Server) Action(action container.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		// 共通のマネージャーを介して非同期または連鎖的なアクション（停止前コマンド等）を実行する。
		if err := s.ContainerManager.ExecuteAction(r.Context(), id, action); err != nil {
			// アクションの失敗は、コンテナの状態不整合やリソース不足などの内部問題（Internal）として扱う。
			logger.Logf("Internal", "API", "コンテナ %s へのアクション %s 実行失敗: %v", id, action, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Logf("Internal", "API", "アクション実行成功: container=%s, action=%s", id, action)
		w.WriteHeader(http.StatusOK)
	}
}

// MARK: CmdContainer()
// コンテナの標準入力(stdin)に対してコマンドを送信する。
func (s *Server) CmdContainer(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")

	// リクエストボディが不正な場合は、クライアント側の誤り（Client）として即座に返却する。
	var payload struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		logger.Logf("Client", "API", "コマンドのデコードに失敗: %v", err)
		http.Error(w, "Invalid Request Body", http.StatusBadRequest)
		return
	}

	// 指定されたコンテナに対して生のコマンド文字列を流し込む。
	if err := docker.SendCommand(id, payload.Command); err != nil {
		// 送信失敗は接続断などの内部的な要因（Internal）として扱う。
		logger.Logf("Internal", "API", "コンテナ %s へのコマンド送信失敗: %v", id, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logger.Logf("Internal", "API", "コマンド送信成功: container=%s, cmd_len=%d", id, len(payload.Command))
	w.WriteHeader(http.StatusOK)
}

// MARK: GetContainerLogs()
// コンテナの過去ログを特定行数取得する。無限スクロール等の用途に使用。
func (s *Server) GetContainerLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	tail := q.Get("tail")
	if tail == "" {
		tail = "100" // デフォルトは直近100行とする
	}

	// 過去のログをスナップショットとして取得する。
	// Follow: false にすることで、ストリーミングではなく現在のバッファのみを取得可能。
	logOptions := ctypes.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
		Tail:       tail,
	}

	logs, err := docker.Client.ContainerLogs(r.Context(), id, logOptions)
	if err != nil {
		logger.Logf("Internal", "API", "過去ログの取得に失敗: container=%s, err=%v", id, err)
		http.Error(w, "Failed to get logs", http.StatusInternalServerError)
		return
	}
	defer logs.Close()

	w.Header().Set("Content-Type", "text/plain")
	// xterm.jsでそのまま扱えるよう、バイナリ（ANSIコード含む）をデマルチプレクスして出力する。
	// TTYが有効な場合はそのままio.Copy可能だが、ログモードでは通常TTYなしとなるためStdCopyを使用。
	inspect, err := docker.Client.ContainerInspect(r.Context(), id)
	if err == nil && inspect.Config.Tty {
		io.Copy(w, logs)
	} else {
		// ヘッダーを除去し、標準出力と標準エラーをマージしてクライアントへ返す。
		// WriteCloserが必要なため、http.ResponseWriterをラップする。
		stdcopy.StdCopy(w, w, logs)
	}
}
