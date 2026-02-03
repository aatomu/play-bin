package main

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

type ReadNullWriteCloser struct {
	r io.ReadCloser
}

func (n *ReadNullWriteCloser) Write(b []byte) (int, error) {
	return io.Discard.Write(b)
}
func (n *ReadNullWriteCloser) Read(b []byte) (int, error) {
	return n.r.Read(b)
}

func (n *ReadNullWriteCloser) Close() error {
	return n.r.Close()
}

func handleTerminal() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, mode := q.Get("id"), q.Get("mode")
		ctx := context.Background()
		var stream io.ReadWriteCloser
		var isTty bool

		// コンテナのTTY設定を事前に確認
		inspect, err := cli.ContainerInspect(ctx, id)
		if err == nil {
			isTty = inspect.Config.Tty
		}

		switch mode {
		case "attach":
			lockMutex.Lock()
			if attachLocks[id] {
				lockMutex.Unlock()
				http.Error(w, "Locked", 409)
				return
			}
			attachLocks[id] = true
			lockMutex.Unlock()
			defer func() { lockMutex.Lock(); delete(attachLocks, id); lockMutex.Unlock() }()

			resp, err := cli.ContainerAttach(ctx, id, container.AttachOptions{
				Stream: true, Stdin: true, Stdout: true, Stderr: true,
			})
			if err != nil {
				return
			}
			stream = resp.Conn
		case "exec":
			isTty = true // Exec時はTTYを強制
			cfg := container.ExecOptions{
				Tty: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
				Env: []string{"TERM=xterm-256color"}, Cmd: []string{"/bin/sh"},
			}
			cExec, err := cli.ContainerExecCreate(ctx, id, cfg)
			if err != nil {
				return
			}
			resp, err := cli.ContainerExecAttach(ctx, cExec.ID, container.ExecAttachOptions{Tty: true})
			if err != nil {
				return
			}
			stream = resp.Conn
		case "logs":
			// logsモードの実装
			logOptions := container.LogsOptions{
				ShowStdout: true,
				ShowStderr: true,
				Follow:     true,
				Timestamps: false,
				Tail:       "all", // 全文表示
			}
			logs, err := cli.ContainerLogs(ctx, id, logOptions)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}

			stream = &ReadNullWriteCloser{
				r: logs,
			}

		}
		defer stream.Close()

		ws, err := upgrader.Upgrade(w, r, nil)
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
				// TTYが有効な場合は、ヘッダーがないため直接コピー可能
				io.Copy(wsWriter, stream)
			} else {
				// TTYが無効（謎の文字が出る原因）な場合、stdcopyを使ってヘッダーを剥がす
				// Minecraftサーバー等、プロセスに直接Attachする場合はこちらが動作する
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

func handleStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()

		stats, err := cli.ContainerStats(context.Background(), id, true)
		if err != nil {
			return
		}
		defer stats.Body.Close()

		io.Copy(&wsBinaryWriter{ws}, stats.Body)
	}
}

func handleContainerAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		var err error

		switch action {
		case "start":
			err = cli.ContainerStart(r.Context(), id, container.StartOptions{})
		case "stop":
			err = cli.ContainerStop(r.Context(), id, container.StopOptions{})
		}

		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}
