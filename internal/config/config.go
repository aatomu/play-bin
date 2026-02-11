package config

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/play-bin/internal/logger"
)

// MARK: LoadedConfig
// プログラム実行中に動的に変更可能な設定情報を管理するスレッドセーフなコンテナ。
type LoadedConfig struct {
	Config
	LastLoaded time.Time
	mu         sync.RWMutex
}

// MARK: Config
// config.json の構造を反映したデータモデル。
type Config struct {
	HTTPListen string                  `json:"httpListen,omitempty"`
	SFTPListen string                  `json:"sftpListen,omitempty"`
	Users      map[string]UserConfig   `json:"users"`
	Servers    map[string]ServerConfig `json:"servers"`
}

type UserConfig struct {
	Discord     string              `json:"discord"`
	Password    string              `json:"password"`
	Permissions map[string][]string `json:"permissions"`
}

// HasPermission checks if the user has the specified permission for the given server.
// It checks specific server permissions first, then wildcards.
func (u UserConfig) HasPermission(serverName, perm string) bool {
	if u.Permissions == nil {
		return false
	}
	// Check specific server permissions
	if perms, ok := u.Permissions[serverName]; ok {
		for _, p := range perms {
			if p == perm {
				return true
			}
		}
	}
	// Check wildcard permissions
	if perms, ok := u.Permissions["*"]; ok {
		for _, p := range perms {
			if p == perm {
				return true
			}
		}
	}
	return false
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
	Mapping map[string]string `json:"mapping"` // bridge時のみ使用
}

type CommandsConfig struct {
	Start   *StartConfig `json:"start,omitempty"`
	Stop    []CmdConfig  `json:"stop,omitempty"`
	Backup  []CmdConfig  `json:"backup,omitempty"`
	Message *string      `json:"message,omitempty"`
}

type StartConfig struct {
	Entrypoint string `json:"entrypoint,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
}

type CmdConfig struct {
	Type string `json:"type"`
	Arg  string `json:"arg"`
}

type DiscordConfig struct {
	Token      string `json:"token"`
	Channel    string `json:"channel"`
	Webhook    string `json:"webhook"`
	LogSetting string `json:"logSetting"`
}

// MARK: Get()
// 現在の設定情報を取得する。
// アクセス毎にファイルの最終更新時刻を検証し、変更があれば透過的にリロードを行う。
func (c *LoadedConfig) Get() Config {
	c.mu.RLock()
	info, err := os.Stat("./config.json")

	if err == nil && info.ModTime().After(c.LastLoaded) {
		// 設定変更を検知したため、共有ロックを解除して書き込みロック（リロード）へ昇格する。
		c.mu.RUnlock()
		c.Reload()
		c.mu.RLock()
	}
	defer c.mu.RUnlock()

	return c.Config
}

// MARK: Reload()
// ディスク上の config.json を読み込み、メモリ上のキャッシュをアトミックに更新する。
func (c *LoadedConfig) Reload() {
	c.mu.Lock()
	defer c.mu.Unlock()

	f, err := os.Open("./config.json")
	if err != nil {
		// ファイル消失やパーミッション不足などの内部的な不整合（Internal）として扱う。
		logger.Logf("Internal", "Config", "設定ファイルのオープンに失敗しました: %v", err)
		return
	}
	defer f.Close()

	var newCfg Config
	if err := json.NewDecoder(f).Decode(&newCfg); err != nil {
		// 不正なJSON形式は、管理者による編集ミスの可能性があるが、システム内処理としてInternalで記録する。
		logger.Logf("Internal", "Config", "設定のパース（JSON）に失敗しました: %v", err)
		return
	}

	c.Config = newCfg
	info, err := f.Stat()
	if err != nil {
		logger.Logf("Internal", "Config", "ファイル情報の取得に失敗しました: %v", err)
		return
	}
	c.LastLoaded = info.ModTime()
	logger.Log("Internal", "Config", "設定ファイルが再読み込みされました")
}
