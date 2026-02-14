package discord

import (
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
)

// BotManager はすべての Discord 連携（Bot操作およびログ転送）のライフサイクルを統合管理する。
type BotManager struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager

	// セッション管理：複数の Bot トークンに対し、個別の常駐セッションを保持する。
	Sessions         map[string]*discordgo.Session
	ChannelToServer  map[string]string
	ChannelUpdatedAt time.Time
	mu               sync.RWMutex

	// ログ転送管理：各コンテナのログ監視プロセスを制御するための情報を保持。
	ActiveForwarders map[string]*forwarderState
	ForwarderMu      sync.RWMutex
}

// MARK: NewBotManager()
// Discord Bot 管理の要となるインスタンスを、依存関係（config, manager）と共に初期化する。
func NewBotManager(cfg *config.LoadedConfig, cm *container.Manager) *BotManager {
	return &BotManager{
		Config:           cfg,
		ContainerManager: cm,
		Sessions:         make(map[string]*discordgo.Session),
		ChannelToServer:  make(map[string]string),
		ActiveForwarders: make(map[string]*forwarderState),
	}
}

// MARK: Start()
// Bot の同期とログ転送管理のバックグラウンドタスクをそれぞれ独立したゴルーチンで起動する。
func (m *BotManager) Start() {
	go m.runBotManager()
	go m.runLogForwarderManager()
}

// MARK: runBotManager()
// 設定ファイルの更新を監視し、Bot の起動・停止・構成変更を動的に反映させるメインループ。
func (m *BotManager) runBotManager() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// 起動時に即座に同期を実行
	m.SyncBots()
	for range ticker.C {
		m.SyncBots()
	}
}

// MARK: runLogForwarderManager()
// コンテナログ転送（Webhook）の有効・無効を、設定変更に合わせてリアルタイムに同期させるループ。
func (m *BotManager) runLogForwarderManager() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	m.SyncLogForwarders()
	for range ticker.C {
		m.SyncLogForwarders()
	}
}
