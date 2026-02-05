package api

import (
	"log"
	"net/http"
	"sync"

	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
)

type Server struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager

	WebSessions  map[string]string // token -> username
	WebSessionMu sync.RWMutex
}

// MARK: NewServer()
func NewServer(cfg *config.LoadedConfig, cm *container.Manager) *Server {
	return &Server{
		Config:           cfg,
		ContainerManager: cm,
		WebSessions:      make(map[string]string),
	}
}

// MARK: Routes()
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// MARK: > File server
	mux.Handle("/", http.FileServer(http.Dir("./")))

	// MARK: > API server
	mux.HandleFunc("/api/login", s.Login)
	mux.HandleFunc("/api/containers", s.Auth(s.ListContainers))
	mux.HandleFunc("/api/container/inspect", s.Auth(s.InspectContainer))
	mux.HandleFunc("/api/container/start", s.Auth(s.Action("start")))
	mux.HandleFunc("/api/container/stop", s.Auth(s.Action("stop")))

	// MARK: > Websocket
	mux.HandleFunc("/ws/terminal", s.Auth(s.TerminalHandler()))
	mux.HandleFunc("/ws/stats", s.Auth(s.StatsHandler()))

	return s.WithLogging(mux)
}

func (s *Server) Start() {
	listen := s.Config.Get().HTTPListen
	if listen == "" {
		log.Println("Http server is disabled (httpListen is empty)")
		return
	}
	log.Printf("Http server has started at \"%s\"", listen)
	log.Fatal(http.ListenAndServe(listen, s.Routes()))
}
