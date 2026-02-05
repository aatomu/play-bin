package discord

import (
	"context"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
)

type BotManager struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager

	Sessions         map[string]*discordgo.Session
	ChannelToServer  map[string]string
	ChannelUpdatedAt time.Time
	mu               sync.RWMutex

	ActiveForwarders map[string]context.CancelFunc
	ForwarderMu      sync.RWMutex
}

// MARK: NewBotManager()
func NewBotManager(cfg *config.LoadedConfig, cm *container.Manager) *BotManager {
	return &BotManager{
		Config:           cfg,
		ContainerManager: cm,
		Sessions:         make(map[string]*discordgo.Session),
		ChannelToServer:  make(map[string]string),
		ActiveForwarders: make(map[string]context.CancelFunc),
	}
}

// MARK: Start()
func (m *BotManager) Start() {
	go m.runBotManager()
	go m.runLogForwarderManager()
}

func (m *BotManager) runBotManager() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	m.SyncBots()
	for range ticker.C {
		m.SyncBots()
	}
}

func (m *BotManager) runLogForwarderManager() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	m.SyncLogForwarders()
	for range ticker.C {
		m.SyncLogForwarders()
	}
}
