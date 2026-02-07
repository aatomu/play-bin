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

	WebSessions  map[string]string // トークン -> ユーザー名
	WebSessionMu sync.RWMutex
}

// MARK: NewServer()
// APIサーバーの新しいインスタンスを作成する。
func NewServer(cfg *config.LoadedConfig, cm *container.Manager) *Server {
	// 各コンポーネントとの依存関係を注入し、セッション管理マップを初期化して返す
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
	// UIの資産である静的ファイル（HTML, CSS, JS）をルートディレクトリから提供する
	mux.Handle("/", http.FileServer(http.Dir("./")))

	// MARK: > Container API
	// ログインやコンテナ一覧、詳細情報取得など、すべての動的APIエンドポイントを定義する
	mux.HandleFunc("/api/login", s.Login)
	mux.HandleFunc("/api/containers", s.Auth(s.ListContainers))
	mux.HandleFunc("/api/container/inspect", s.Auth(s.InspectContainer))
	mux.HandleFunc("/api/container/start", s.Auth(s.Action("start")))
	mux.HandleFunc("/api/container/stop", s.Auth(s.Action("stop")))

	// MARK: > WebSocket API
	// ターミナルの入力同期やリソース使用率のリアルタイム配信のためにWebSocketを利用する
	mux.HandleFunc("/ws/terminal", s.Auth(s.TerminalHandler()))
	mux.HandleFunc("/ws/stats", s.Auth(s.StatsHandler()))

	return s.WithLogging(mux)
}

// MARK: Start()
// HTTPサーバーを起動し、リクエストの待機を開始する。
func (s *Server) Start() {
	addr := s.Config.Get().HTTPListen
	if addr == "" {
		// 設定が空の場合は機能を提供できないため、起動をスキップする
		logger.Log("Internal", "API", "HTTPサーバーは無効です（httpListenが未設定）")
		return
	}
	logger.Logf("Internal", "API", "HTTPサーバーが開始されました: \"%s\"", addr)
	// 指定されたアドレスでリスニングを開始。エラーが発生した場合はプロセスを終了させる。
	if err := http.ListenAndServe(addr, s.Routes()); err != nil {
		logger.Logf("Internal", "API", "HTTPサーバーが予期せず終了しました: %v", err)
		panic(err)
	}
}
