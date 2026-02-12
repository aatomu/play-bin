package api

import (
	"net/http"
	"sync"

	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/logger"
)

// Server はAPIサーバーの本体を表す。
type Server struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager

	// WebSessions はトークンをキー、ユーザー名を値として管理するスレッドセーフなマップ。
	WebSessions  map[string]string
	WebSessionMu sync.RWMutex
}

// MARK: NewServer()
// APIサーバーの新しいインスタンスを作成する。
func NewServer(cfg *config.LoadedConfig, cm *container.Manager) *Server {
	// 各コンポーネントとの依存関係を明示的に注入し、整合性を保った状態でインスタンスを初期化する。
	return &Server{
		Config:           cfg,
		ContainerManager: cm,
		WebSessions:      make(map[string]string),
	}
}

// MARK: Routes()
// ルーティング設定（HTTPハンドラーの登録）を行う。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// MARK: > Static Files
	// UIの資産である静的ファイル（HTML, CSS, JS）をルートディレクトリから提供する。
	mux.Handle("/", http.FileServer(http.Dir("./")))

	// MARK: > Container API
	// ログインやコンテナ一覧、詳細情報取得など、すべての動的APIエンドポイントを定義する。
	// Authミドルウェアを介することで、未認証ユーザーによる操作を未然に防ぐ。
	mux.HandleFunc("/api/login", s.Login)
	mux.HandleFunc("/api/containers", s.Auth(s.ListContainers))
	mux.HandleFunc("/api/container/inspect", s.Auth(s.InspectContainer))
	mux.HandleFunc("/api/container/start", s.Auth(s.Action("start")))
	mux.HandleFunc("/api/container/stop", s.Auth(s.Action("stop")))
	mux.HandleFunc("/api/container/kill", s.Auth(s.Action("kill")))
	mux.HandleFunc("/api/container/backup", s.Auth(s.Action("backup")))
	mux.HandleFunc("/api/container/restore", s.Auth(s.Action("restore")))
	mux.HandleFunc("/api/container/remove", s.Auth(s.Action("remove")))
	mux.HandleFunc("/api/container/cmd", s.Auth(s.CmdContainer))
	mux.HandleFunc("/api/container/logs", s.Auth(s.GetContainerLogs))

	// MARK: > WebSocket API
	// ターミナルの入力同期やリソース使用率のリアルタイム配信のためにWebSocketを利用する。
	mux.HandleFunc("/ws/terminal", s.Auth(s.TerminalHandler()))
	mux.HandleFunc("/ws/stats", s.Auth(s.StatsHandler()))

	// 全てのリクエストに対してアクセスログを出力する共通ラッパーを適用する。
	return s.WithLogging(mux)
}

// MARK: Start()
// HTTPサーバーを起動し、リクエストの待機を開始する。
func (s *Server) Start() {
	addr := s.Config.Get().HTTPListen
	if addr == "" {
		// 待機アドレスが未設定の場合は、APIサービスを提供しない意図と判断し起動をスキップする。
		logger.Log("Internal", "API", "HTTPサーバーは無効です（httpListenが未設定）")
		return
	}
	logger.Logf("Internal", "API", "HTTPサーバーが開始されました: \"%s\"", addr)

	// 指定されたアドレスでリスニングを開始。
	// エラーが発生した場合は致命的なシステム障害（ポート競合等）と見なし、プロセスを停止させる。
	if err := http.ListenAndServe(addr, s.Routes()); err != nil {
		logger.Logf("Internal", "API", "HTTPサーバーが予期せず終了しました: %v", err)
		panic(err)
	}
}
