package api

import (
	"io"
	"net/http"
	"sync"

	ctypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gorilla/websocket"
	"github.com/play-bin/internal/docker"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// MARK: TerminalHandler
func (s *Server) TerminalHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, mode := q.Get("id"), q.Get("mode")
		ctx := r.Context()
		var stream io.ReadWriteCloser
		var isTty bool

		inspect, err := docker.Client.ContainerInspect(ctx, id)
		if err == nil {
			isTty = inspect.Config.Tty
		}

		switch mode {
		case "attach":
			docker.AttachLockMu.Lock()
			if docker.AttachLocks[id] {
				docker.AttachLockMu.Unlock()
				http.Error(w, "Locked", 409)
				return
			}
			docker.AttachLocks[id] = true
			docker.AttachLockMu.Unlock()
			defer func() {
				docker.AttachLockMu.Lock()
				delete(docker.AttachLocks, id)
				docker.AttachLockMu.Unlock()
			}()

			resp, err := docker.Client.ContainerAttach(ctx, id, ctypes.AttachOptions{
				Stream: true, Stdin: true, Stdout: true, Stderr: true,
			})
			if err != nil {
				return
			}
			stream = resp.Conn
		case "exec":
			isTty = true
			cfg := ctypes.ExecOptions{
				Tty: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
				Env: []string{"TERM=xterm-256color"}, Cmd: []string{"/bin/sh"},
			}
			cExec, err := docker.Client.ContainerExecCreate(ctx, id, cfg)
			if err != nil {
				return
			}
			resp, err := docker.Client.ContainerExecAttach(ctx, cExec.ID, ctypes.ExecAttachOptions{Tty: true})
			if err != nil {
				return
			}
			stream = resp.Conn
		case "logs":
			logOptions := ctypes.LogsOptions{
				ShowStdout: true, ShowStderr: true, Follow: true, Tail: "all",
			}
			logs, err := docker.Client.ContainerLogs(ctx, id, logOptions)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			stream = &docker.ReadNullWriteCloser{R: logs}
		}

		if stream != nil {
			defer stream.Close()
		}

		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		var once sync.Once
		done := make(chan struct{})
		cleanup := func() {
			once.Do(func() {
				close(done)
				if stream != nil {
					stream.Close()
				}
				ws.Close()
			})
		}

		// Docker -> WebSocket
		go func() {
			defer cleanup()
			wsWriter := &wsBinaryWriter{ws}
			if isTty {
				io.Copy(wsWriter, stream)
			} else {
				stdcopy.StdCopy(wsWriter, wsWriter, stream)
			}
		}()

		// WebSocket -> Docker
		go func() {
			defer cleanup()
			for {
				_, msg, err := ws.ReadMessage()
				if err != nil {
					return
				}
				stream.Write(msg)
			}
		}()

		<-done
	}
}

// MARK: StatsHandler
func (s *Server) StatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		ws, _ := wsUpgrader.Upgrade(w, r, nil)
		defer ws.Close()

		stats, err := docker.Client.ContainerStats(r.Context(), id, true)
		if err != nil {
			return
		}
		defer stats.Body.Close()
		io.Copy(&wsBinaryWriter{ws}, stats.Body)
	}
}
