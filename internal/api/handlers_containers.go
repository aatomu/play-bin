package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	ctypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

// ContainerListItem はリスト表示用のコンテナ情報を表す。
type ContainerListItem struct {
	ID          string   `json:"id"`
	Names       []string `json:"names"`
	State       string   `json:"state"`       // running, stopped, missing
	Actions     []string `json:"actions"`     // Available actions based on permission and config
	Permissions []string `json:"permissions"` // "read", "write", "execute"
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
	for serverName, serverCfg := range cfg.Servers {
		// 権限がないサーバーはリスト自体に表示させない。
		if !user.HasPermission(serverName, "read") {
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

		// 利用可能なアクションを計算する
		item.Actions = s.calculateActions(user, serverName, serverCfg)
		// 権限リストも付与する（フロントエンドでのボタン制御用）
		if user.HasPermission(serverName, "read") {
			item.Permissions = append(item.Permissions, "read")
		}
		if user.HasPermission(serverName, "write") {
			item.Permissions = append(item.Permissions, "write")
		}
		if user.HasPermission(serverName, "execute") {
			item.Permissions = append(item.Permissions, "execute")
		}
		result = append(result, item)
	}

	// 2. 設定ファイルにはないが、Docker上に存在する（かつ権限のある）コンテナを追加する。
	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		name := c.Names[0][1:]
		if processedDockerNames[name] || !user.HasPermission(name, "read") {
			continue
		}

		// 設定ファイルにないコンテナはアクションを持たない（制御不能）
		item := ContainerListItem{
			ID:    c.ID,
			Names: c.Names,
			State: c.State,
		}
		// 権限リストも付与
		if user.HasPermission(name, "read") {
			item.Permissions = append(item.Permissions, "read")
		}
		if user.HasPermission(name, "write") {
			item.Permissions = append(item.Permissions, "write")
		}
		if user.HasPermission(name, "execute") {
			item.Permissions = append(item.Permissions, "execute")
		}
		result = append(result, item)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		// JSON変換の失敗はプログラムの不備（Internal）として扱う。
		logger.Logf("Internal", "API", "JSONエンコード失敗: %v", err)
	}
}

// MARK: calculateActions()
// ユーザー権限とサーバー設定に基づいて、実行可能なアクションのリストを生成する。
func (s *Server) calculateActions(user config.UserConfig, name string, cfg config.ServerConfig) []string {
	// execute権限がない場合はアクションなし
	if !user.HasPermission(name, "execute") {
		return []string{}
	}

	var actions []string

	// 設定ファイルに定義が存在する場合のみ追加
	if cfg.Compose != nil && cfg.Compose.Image != "" {
		actions = append(actions, "start")
	}
	if cfg.Commands.Stop != nil { // 停止定義があれば Stop と Kill を許可
		actions = append(actions, "stop")
		actions = append(actions, "kill")
	}
	if len(cfg.Commands.Backup) > 0 {
		actions = append(actions, "backup")
		actions = append(actions, "restore")
	}

	// 物理的なコンテナが存在する場合のみ、削除(remove)を許可する
	actions = append(actions, "remove")

	return actions
}

// MARK: InspectContainer()
// コンテナの詳細情報を取得する。
func (s *Server) InspectContainer(w http.ResponseWriter, r *http.Request) {
	serverName := r.URL.Query().Get("id")
	// 詳細情報を取得し、フロントエンドでの詳細表示（スペックやネットワーク設定など）に利用する。
	inspect, err := docker.Client.ContainerInspect(r.Context(), serverName)
	if err != nil {
		// コンテナが見つからない原因はクライアントからの無効な指定（Client）として扱う。
		logger.Logf("Client", "API", "コンテナ %s の詳細取得失敗: %v", serverName, err)
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
		serverName := r.URL.Query().Get("id")

		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		s.WebSessionMu.RLock()
		username := s.WebSessions[token]
		s.WebSessionMu.RUnlock()

		if !s.Config.Get().Users[username].HasPermission(serverName, "execute") {
			logger.Logf("Client", "API", "Action拒否: user=%s, target=%s", username, serverName)
			http.Error(w, "Execute permission required", http.StatusForbidden)
			return
		}

		// バックアップ・リストア等の長時間処理に対応するため、HTTPリクエストのコンテキストではなく、
		// 十分なタイムアウトを持つ背景コンテキストを使用する。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		// 共通のマネージャーを介して非同期または連鎖的なアクション（停止前コマンド等）を実行する。
		if err := s.ContainerManager.ExecuteAction(ctx, serverName, action); err != nil {
			// アクションの失敗は、コンテナの状態不整合やリソース不足などの内部問題（Internal）として扱う。
			logger.Logf("Internal", "API", "コンテナ %s へのアクション %s 実行失敗: %v", serverName, action, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logger.Logf("Internal", "API", "アクション実行成功: container=%s, action=%s", serverName, action)
		w.WriteHeader(http.StatusOK)
	}
}

// MARK: ListBackups()
// 指定コンテナのバックアップ世代一覧を返す。
func (s *Server) ListBackups(w http.ResponseWriter, r *http.Request) {
	serverName := r.URL.Query().Get("id")

	generations, err := s.ContainerManager.ListBackupGenerations(serverName)
	if err != nil {
		logger.Logf("Internal", "API", "バックアップ世代一覧取得失敗: container=%s, err=%v", serverName, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(generations); err != nil {
		logger.Logf("Internal", "API", "JSONエンコード失敗: %v", err)
	}
}

// MARK: RestoreAction()
// 世代を指定してコンテナのバックアップから復元するハンドラー。
func (s *Server) RestoreAction(w http.ResponseWriter, r *http.Request) {
	serverName := r.URL.Query().Get("id")
	generation := r.URL.Query().Get("generation")

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	s.WebSessionMu.RLock()
	username := s.WebSessions[token]
	s.WebSessionMu.RUnlock()

	if !s.Config.Get().Users[username].HasPermission(serverName, "execute") {
		logger.Logf("Client", "API", "Restore拒否: user=%s, target=%s", username, serverName)
		http.Error(w, "Execute permission required", http.StatusForbidden)
		return
	}

	// バックアップ・リストア等の長時間処理に対応するため、HTTPリクエストのコンテキストではなく、
	// 十分なタイムアウトを持つ背景コンテキストを使用する。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// 世代パラメータを受けて直接 Restore を呼び出す。
	if err := s.ContainerManager.Restore(ctx, serverName, generation); err != nil {
		logger.Logf("Internal", "API", "コンテナ %s のリストア失敗 (generation=%s): %v", serverName, generation, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logger.Logf("Internal", "API", "リストア成功: container=%s, generation=%s", serverName, generation)
	w.WriteHeader(http.StatusOK)
}

// MARK: CmdContainer()
// コンテナの標準入力(stdin)に対してコマンドを送信する。
func (s *Server) CmdContainer(w http.ResponseWriter, r *http.Request) {
	serverName := r.URL.Query().Get("id")

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	s.WebSessionMu.RLock()
	username := s.WebSessions[token]
	s.WebSessionMu.RUnlock()

	if !s.Config.Get().Users[username].HasPermission(serverName, "write") {
		logger.Logf("Client", "API", "Cmd拒否: user=%s, target=%s", username, serverName)
		http.Error(w, "Write permission required", http.StatusForbidden)
		return
	}

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
	if err := docker.SendCommand(serverName, payload.Command); err != nil {
		// 送信失敗は接続断などの内部的な要因（Internal）として扱う。
		logger.Logf("Internal", "API", "コンテナ %s へのコマンド送信失敗: %v", serverName, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logger.Logf("Internal", "API", "コマンド送信成功: container=%s, cmd_len=%d", serverName, len(payload.Command))
	w.WriteHeader(http.StatusOK)
}

// MARK: GetContainerLogs()
// コンテナの過去ログを特定行数取得する。無限スクロール等の用途に使用。
func (s *Server) GetContainerLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverName := q.Get("id")
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

	logs, err := docker.Client.ContainerLogs(r.Context(), serverName, logOptions)
	if err != nil {
		logger.Logf("Internal", "API", "過去ログの取得に失敗: container=%s, err=%v", serverName, err)
		http.Error(w, "Failed to get logs", http.StatusInternalServerError)
		return
	}
	defer logs.Close()

	w.Header().Set("Content-Type", "text/plain")
	// xterm.jsでそのまま扱えるよう、バイナリ（ANSIコード含む）をデマルチプレクスして出力する。
	// TTYが有効な場合はそのままio.Copy可能だが、ログモードでは通常TTYなしとなるためStdCopyを使用。
	inspect, err := docker.Client.ContainerInspect(r.Context(), serverName)
	if err == nil && inspect.Config.Tty {
		io.Copy(w, logs)
	} else {
		// ヘッダーを除去し、標準出力と標準エラーをマージしてクライアントへ返す。
		// WriteCloserが必要なため、http.ResponseWriterをラップする。
		stdcopy.StdCopy(w, w, logs)
	}
}
