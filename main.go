package main

import (
	"log"

	"github.com/play-bin/internal/api"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/discord"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/sftp"
)

// MARK: main()
func main() {
	// MARK: Config
	cfg := &config.LoadedConfig{}
	cfg.Reload()

	// MARK: Docker client
	log.Println("Docker client to be starting...")
	if err := docker.Init(); err != nil {
		log.Fatalf("Docker client failed to start: %v", err)
	}
	log.Println("Docker client has started")

	// MARK: Manager & Services
	cm := &container.Manager{Config: cfg}
	ds := discord.NewBotManager(cfg, cm)
	as := api.NewServer(cfg, cm)
	ss := sftp.NewServer(cfg, cm)

	// MARK: Discord Bots & Log Forwarders
	log.Println("Discord services starting...")
	ds.Start()
	log.Println("Discord services started")

	// MARK: SFTP server
	log.Println("SFTP server starting...")
	go ss.Start()

	// MARK: Http server
	log.Println("Http server starting...")
	as.Start()
}
