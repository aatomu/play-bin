package config

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// MARK: Config
type LoadedConfig struct {
	Config
	LastLoaded time.Time
	mu         sync.RWMutex
}

type Config struct {
	HTTPListen string                  `json:"httpListen,omitempty"`
	SFTPListen string                  `json:"sftpListen,omitempty"`
	Users      map[string]UserConfig   `json:"users"`
	Servers    map[string]ServerConfig `json:"servers"`
}

type UserConfig struct {
	Discord      string   `json:"discord"`
	Password     string   `json:"password"`
	Controllable []string `json:"controllable"`
}

type ServerConfig struct {
	WorkingDir string            `json:"workingDir"`
	Image      string            `json:"image"`
	Network    NetworkConfig     `json:"network"`
	Mount      map[string]string `json:"mount"`
	Commands   CommandsConfig    `json:"commands"`
	Discord    DiscordConfig     `json:"discord"`
}

type NetworkConfig struct {
	Mode    string            `json:"mode"`    // "host" or "bridge"
	Mapping map[string]string `json:"mapping"` // use on "bridge" mode
}

type CommandsConfig struct {
	Start   StartConfig    `json:"start"`
	Stop    []CmdConfig    `json:"stop"`
	Backup  []BackupConfig `json:"backup"`
	Message string         `json:"message"`
}

type StartConfig struct {
	Entrypoint string `json:"entrypoint,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
}

type CmdConfig struct {
	Type string `json:"type"`
	Arg  string `json:"arg"`
}

type BackupConfig struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

type DiscordConfig struct {
	Token      string `json:"token"`
	Channel    string `json:"channel"`
	Webhook    string `json:"webhook"`
	LogSetting string `json:"logSetting"`
}

// MARK: Get()
// 設定を取得。変更があれば自動で再読み込み
func (c *LoadedConfig) Get() Config {
	c.mu.RLock()
	info, err := os.Stat("./config.json")

	if err == nil && info.ModTime().After(c.LastLoaded) {
		c.mu.RUnlock()
		c.Reload()
		c.mu.RLock()
	}
	defer c.mu.RUnlock()

	return c.Config
}

// MARK: Reload()
// 設定ファイルを読み込む
func (c *LoadedConfig) Reload() {
	c.mu.Lock()
	defer c.mu.Unlock()

	f, err := os.Open("./config.json")
	if err != nil {
		log.Printf("Config load failed: %v", err)
		return
	}
	defer f.Close()

	var newCfg Config
	if err := json.NewDecoder(f).Decode(&newCfg); err != nil {
		log.Printf("Config parse failed: %v", err)
		return
	}

	c.Config = newCfg
	info, err := f.Stat()
	if err != nil {
		log.Printf("Config stat failed: %v", err)
		return
	}
	c.LastLoaded = info.ModTime()
	log.Println("Config has reloaded")
}
