package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// MARK: Config
type loadedConfig struct {
	Config
	lastLoaded time.Time
	mu         sync.RWMutex
}
type Config struct {
	Listen  string                  `json:"listen"`
	Users   map[string]ConfigUser   `json:"users"`
	Servers map[string]ConfigServer `json:"servers"`
}

type ConfigUser struct {
	Discord      string   `json:"discord"`
	Password     string   `json:"password"`
	Controllable []string `json:"controllable"`
}

type ConfigServer struct {
	WorkingDir string            `json:"workingDir"`
	Image      string            `json:"image"`
	Network    ConfigNetwork     `json:"network"`
	Mount      map[string]string `json:"mount"`
	Commands   ConfigCommands    `json:"commands"`
	Discord    ConfigDiscord     `json:"discord"`
}

type ConfigNetwork struct {
	Mode    string            `json:"mode"`
	Mapping map[string]string `json:"mapping"`
}

type ConfigCommands struct {
	Start   string         `json:"start"`
	Stop    []ConfigCmd    `json:"stop"`
	Backup  []ConfigBackup `json:"backup"`
	Message string         `json:"message"`
}

type ConfigCmd struct {
	Mode string `json:"mode"`
	Arg  string `json:"arg"`
}

type ConfigBackup struct {
	Src  string `json:"src"`
	Dest string `json:"dest"`
}

type ConfigDiscord struct {
	Token      string `json:"token"`
	Channel    string `json:"channel"`
	Webhook    string `json:"webhook"`
	LogSetting string `json:"logSetting"`
}

func getConfig() Config {
	lc.mu.RLock()
	info, err := os.Stat("./config.json")

	if err == nil && info.ModTime().After(lc.lastLoaded) {
		lc.mu.RUnlock()
		updateConfig(info.ModTime())
		lc.mu.RLock()
	}
	defer lc.mu.RUnlock()

	return lc.Config
}

func updateConfig(modTime time.Time) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	if !modTime.After(lc.lastLoaded) {
		return
	}

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

	lc.Config = newCfg
	lc.lastLoaded = modTime
	log.Println("Config has reloaded")
}
