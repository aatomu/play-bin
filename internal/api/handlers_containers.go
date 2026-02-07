package api

import (
	"encoding/json"
	"net/http"

	ctypes "github.com/docker/docker/api/types/container"
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
	// 現在のDocker上の全コンテナと管理対象設定を突き合わせるため、まずDockerから情報を取得する
	containers, err := docker.Client.ContainerList(r.Context(), ctypes.ListOptions{All: true})
	if err != nil {
		logger.Logf("Internal", "API", "コンテナリストの取得に失敗: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// 突き合わせの効率化のため、Docker上のコンテナを名前をキーとしたマップに整理する
	dockerMap := make(map[string]ctypes.Summary)
	for _, c := range containers {
		for _, name := range c.Names {
			// 先頭の '/' を除去して比較しやすくする
			dockerMap[name[1:]] = c
		}
	}

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.URL.Query().Get("token")
	}

	// ユーザーごとの権限に基づいたフィルタリングを行うため、セッションからユーザー情報を特定する
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
		// 権限がないサーバーはリスト自体に表示させない
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
		logger.Error("API", err)
	}
}

// MARK: isAllowed()
// ユーザーが特定のコンテナ名に対する操作権限を持っているか検証する。
func (s *Server) isAllowed(user config.UserConfig, name string) bool {
	// 設定ミスなどでユーザー情報が空の場合の安全策
	if user.Controllable == nil {
		return false
	}
	for _, pattern := range user.Controllable {
		// ワイルドカード指定または名前が完全一致する場合に許可する
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
	// 詳細情報を取得し、フロントエンドでの詳細表示（スペックやネットワーク設定など）に利用する
	inspect, err := docker.Client.ContainerInspect(r.Context(), id)
	if err != nil {
		// コンテナが見つからない場合はクライアント側の状態不整合の可能性があるため404を返す
		logger.Logf("Client", "API", "コンテナ %s の詳細取得失敗: %v", id, err)
		http.Error(w, "Container Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(inspect); err != nil {
		logger.Error("API", err)
	}
}

// MARK: Action()
// コンテナに対して操作（起動・停止など）を実行するためのハンドラーを生成する。
func (s *Server) Action(action container.Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		// 共通のマネージャーを介して非同期または連鎖的なアクション（停止前コマンド等）を実行する
		if err := s.ContainerManager.ExecuteAction(r.Context(), id, action); err != nil {
			logger.Logf("Internal", "API", "コンテナ %s へのアクション %s 実行失敗: %v", id, action, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
