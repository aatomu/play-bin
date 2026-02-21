package config

import (
	"encoding/json"
	"os"
	"strings"
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
	Discord     string              `json:"discord,omitempty"`
	Password    string              `json:"password"`
	Permissions map[string][]string `json:"permissions"`
}

const (
	// File permissions
	PermFileRead  = "file.read"
	PermFileWrite = "file.write"

	// Container permissions
	PermContainerRead    = "container.read"
	PermContainerWrite   = "container.write"
	PermContainerExecute = "container.execute.*"

	PermContainerStart   = "container.execute.start"
	PermContainerStop    = "container.execute.stop"
	PermContainerKill    = "container.execute.kill"
	PermContainerBackup  = "container.execute.backup"
	PermContainerRestore = "container.execute.restore"
	PermContainerRemove  = "container.execute.remove"
)

// HasPermission checks if the user has the specified permission for the given server.
// It supports hierarchical permissions with wildcards (e.g., "container.*" matches "container.read").
func (u UserConfig) HasPermission(serverName, requiredPerm string) bool {
	if u.Permissions == nil {
		return false
	}

	// 1. Check specific server permissions
	if checkPermission(u.Permissions[serverName], requiredPerm) {
		return true
	}

	// 2. Check wildcard server permissions
	if checkPermission(u.Permissions["*"], requiredPerm) {
		return true
	}

	return false
}

// checkPermission performs hierarchical wildcard matching for a list of permissions.
func checkPermission(userPerms []string, required string) bool {
	for _, p := range userPerms {
		if matchRecursive(p, required) {
			return true
		}
	}
	return false
}

// matchRecursive compare user permission and required permission with dot notation and wildcard support.
// e.g., "container.*" matches "container.read"
func matchRecursive(userPerm, required string) bool {
	if userPerm == "*" {
		return true
	}
	if userPerm == required {
		return true
	}

	userParts := strings.Split(userPerm, ".")
	reqParts := strings.Split(required, ".")

	for i := 0; i < len(userParts); i++ {
		if userParts[i] == "*" {
			return true
		}
		if i >= len(reqParts) || userParts[i] != reqParts[i] {
			return false
		}
	}

	// If userPerm finished without wildcard but reqPerm is longer, it's not a match.
	// unless userPerm is exactly reqPerm (handled above).
	return len(userParts) == len(reqParts)
}

type ServerConfig struct {
	WorkingDir string         `json:"workingDir,omitempty"`
	Compose    *ComposeConfig `json:"compose,omitempty"`
	Commands   CommandsConfig `json:"commands"`
	Discord    *DiscordConfig `json:"discord,omitempty"`
}

type ComposeConfig struct {
	Image   string            `json:"image"`
	Restart string            `json:"restart,omitempty"`
	Command *StartConfig      `json:"command,omitempty"`
	Network NetworkConfig     `json:"network,omitempty"`
	Mount   map[string]string `json:"mount,omitempty"`
}

type NetworkConfig struct {
	Mode    string            `json:"mode"`    // "host" or "bridge"
	Mapping map[string]string `json:"mapping"` // bridge時のみ使用
}

type CommandsConfig struct {
	Stop    []CmdConfig `json:"stop,omitempty"`
	Backup  []CmdConfig `json:"backup,omitempty"`
	Message *string     `json:"message,omitempty"`
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
	Token      string `json:"token,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Webhook    string `json:"webhook,omitempty"`
	LogSetting string `json:"logSetting,omitempty"`
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
