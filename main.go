package main

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

// MARK: var
var (
	// Docker
	dockerCli    *client.Client
	attachLocks  = make(map[string]bool)
	attachLockMu sync.Mutex

	// Web client
	webSessions  = make(map[string]string)
	webSessionMu sync.RWMutex
	// Websocket
	wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// Discord
	discordSessions  = make(map[string]*discordgo.Session) // token -> session
	channelToServer  = make(map[string]string)             // ChannelID -> ServerName
	channelUpdatedAt time.Time                             // 最後に更新した時間
	discordMu        sync.RWMutex

	// Other
	config loadedConfig
)

// MARK: main()
func main() {
	var err error
	// MARK: Docker client
	log.Println("Docker client to be starting...")
	dockerCli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Docker client failed to start: %v", err)
	}
	log.Println("Docker client has started")

	// MARK: Discord Bots
	log.Println("Discord bots to be starting...")
	go startDiscordBots()
	log.Println("Discord bots has started")
	log.Println("Log forwarders to be starting...")
	go startLogForwarders()
	log.Println("Log forwarders has started")

	// MARK: Http server
	log.Println("Http server to be starting...")
	mux := http.NewServeMux()
	// MARK: > File server
	mux.Handle("/", http.FileServer(http.Dir("./")))
	// MARK: > API server
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/containers", auth(handleContainerList))
	mux.HandleFunc("/api/container/inspect", auth(handleContainerInspect))
	mux.HandleFunc("/api/container/start", auth(handleContainerAction("start")))
	mux.HandleFunc("/api/container/stop", auth(handleContainerAction("stop")))
	// MARK: > Websocket
	mux.HandleFunc("/ws/terminal", auth(handleTerminal()))
	mux.HandleFunc("/ws/stats", auth(handleStats()))

	listen := config.Get().Listen
	log.Printf("Http server has started at \"%s\"", listen)
	log.Fatal(http.ListenAndServe(listen, middleware(mux)))
}
