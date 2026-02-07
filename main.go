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
	// 起動時に最新の設定をメモリに展開し、以降のコンポーネントで参照可能にする。
	cfg := &config.LoadedConfig{}
	cfg.Reload()

	// MARK: > Docker Client
	// Dockerエンジンとの通信が確立できないと全ての操作が不可能になるため、最初期に検証する。
	logger.Log("Internal", "System", "Dockerクライアントを初期化しています...")
	if err := docker.Init(); err != nil {
		// 接続失敗時は続行不能と判断し、原因を明示してプロセスの起動を中断する。
		logger.Error("System", err)
		return
	}
	logger.Log("Internal", "System", "Dockerクライアントが準備完了しました")

	// MARK: > Initialize Services
	// 各サービスが相互に依存する設定やマネージャーを注入し、インスタンスを生成する。
	cm := &container.Manager{Config: cfg}
	ds := discord.NewBotManager(cfg, cm)
	as := api.NewServer(cfg, cm)
	ss := sftp.NewServer(cfg, cm)

	// MARK: > Start Background Services
	// 非ブロッキングで動作させる必要のあるサービスを非同期(または専用ループ)で開始する。
	logger.Log("Internal", "Discord", "Discord連携サービスを開始しています...")
	ds.Start()

	logger.Log("Internal", "SFTP", "SFTPサーバーを開始しています...")
	go ss.Start()

	// MARK: > Start Web Server
	// HTTPサーバーはリクエスト待機のためにメインスレッドを占有(ブロッキング)するため、最後に配置する。
	logger.Log("Internal", "API", "Webサーバーを開始しています...")
	as.Start()
}
