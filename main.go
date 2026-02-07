package main

import (
	"github.com/play-bin/internal/api"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/discord"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
	"github.com/play-bin/internal/sftp"
)

// MARK: main()
// アプリケーションの基盤システム（設定、Docker、各サービス）を初期化し起動する。
func main() {
	// MARK: > Initialize Config
	cfg := &config.LoadedConfig{}
	cfg.Reload() // 起動時に一度強制的に読み込む

	// MARK: > Docker Client
	logger.Log("Internal", "System", "Dockerクライアントを初期化しています...")
	if err := docker.Init(); err != nil {
		// Dockerが動いていない場合はシステム全体が機能しないため、致命的エラーとして扱う
		logger.Error("System", err)
		return
	}
	logger.Log("Internal", "System", "Dockerクライアントが準備完了しました")

	// MARK: > Initialize Services
	cm := &container.Manager{Config: cfg}
	ds := discord.NewBotManager(cfg, cm)
	as := api.NewServer(cfg, cm)
	ss := sftp.NewServer(cfg, cm)

	// MARK: > Start Background Services
	logger.Log("Internal", "Discord", "Discord連携サービスを開始しています...")
	ds.Start()

	logger.Log("Internal", "SFTP", "SFTPサーバーを開始しています...")
	go ss.Start()

	// MARK: > Start Web Server
	// HTTPサーバーはブロッキング実行されるため、最後に配置する
	logger.Log("Internal", "API", "Webサーバーを開始しています...")
	as.Start()
}
