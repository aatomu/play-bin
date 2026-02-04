package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

// MARK: var
var (
	wsUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	cli         *client.Client
	sessions    = make(map[string]string)
	sessionLock sync.RWMutex
	attachLocks = make(map[string]bool)
	lockMutex   sync.Mutex

	lc = &loadedConfig{}
)

// MARK: main()
func main() {
	var err error
	// MARK: Docker client
	cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Docker接続失敗: %v", err)
	}

	// MARK: Discord Bots
	go startDiscordBots()
	go startLogForwarders()

	// MARK: Http server
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

	listen := getConfig().Listen
	log.Printf("Console Server started at \"%s\"", listen)
	log.Fatal(http.ListenAndServe(listen, middleware(mux)))
}
