package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/moby/moby/client"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	// セッション管理 (Token -> Username)
	sessions    = make(map[string]string)
	sessionLock sync.RWMutex
	// Attach排他制御 (ContainerID -> isLocked)
	attachLocks = make(map[string]bool)
	lockMutex   sync.Mutex
)

func main() {
	// Dockerクライアントの初期化 (環境変数 DOCKER_HOST 等を参照)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Docker接続失敗: %v", err)
	}

	// --- 静的ファイル配信 ---
	http.Handle("/", http.FileServer(http.Dir("./")))

	// --- REST API ---
	// ログイン
	http.HandleFunc("/api/login", handleLogin)
	// コンテナ一覧
	http.HandleFunc("/api/containers", auth(func(w http.ResponseWriter, r *http.Request) {
		containers, err := cli.ContainerList(r.Context(), client.ContainerListOptions{All: true})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(containers)
	}))
	// 詳細情報取得 (Inspect)
	http.HandleFunc("/api/container/inspect", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		inspect, err := cli.ContainerInspect(r.Context(), id, client.ContainerInspectOptions{})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(inspect)
	}))

	// --- WebSockets ---
	// ターミナル (Attach / Exec)
	http.HandleFunc("/ws/terminal", handleTerminal(cli))
	// リソース監視 (Stats)
	http.HandleFunc("/ws/stats", handleStats(cli))

	fmt.Println("Admin Console Server started at :1031")
	log.Fatal(http.ListenAndServe(":1031", nil))
}

// 認証ミドルウェア (HeaderまたはURLクエリからトークンを確認)
func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		sessionLock.RLock()
		_, ok := sessions[token]
		sessionLock.RUnlock()
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ログイン処理: ランダムなトークンを生成してメモリに保持
func handleLogin(w http.ResponseWriter, r *http.Request) {
	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	sessionLock.Lock()
	sessions[token] = "admin" // 簡易的に admin 固定
	sessionLock.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// ターミナル制御 (WS Bridge)
func handleTerminal(cli *client.Client) http.HandlerFunc {
	return auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, mode := q.Get("id"), q.Get("mode")
		ctx := context.Background()
		var stream net.Conn

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

			resp, err := cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
				Stream: true, Stdin: true, Stdout: true, Stderr: true,
			})
			if err != nil {
				return
			}
			stream = resp.Conn
		case "exec":
			cfg := client.ExecCreateOptions{
				TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
				Env: []string{"TERM=xterm-256color"}, Cmd: []string{"/bin/sh"},
			}
			cExec, err := cli.ExecCreate(ctx, id, cfg)
			if err != nil {
				return
			}
			resp, err := cli.ExecAttach(ctx, cExec.ID, client.ExecAttachOptions{TTY: true})
			if err != nil {
				return
			}
			stream = resp.Conn
		}
		defer stream.Close()

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		// パニック回避用の Once と 同期用チャネル
		var once sync.Once
		done := make(chan struct{})

		// 片付け用の関数
		cleanup := func() {
			once.Do(func() {
				close(done)
				stream.Close() // ここでストリームを閉じれば読み取りループも終わる
			})
		}

		// Docker -> WebSocket
		go func() {
			defer cleanup()
			io.Copy(&wsBinaryWriter{ws}, stream)
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

		// どちらかが終わるまで待機
		<-done
	})
}

// リソース統計 (Docker Stats APIをWSで中継)
func handleStats(cli *client.Client) http.HandlerFunc {
	return auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		ws, _ := upgrader.Upgrade(w, r, nil)
		defer ws.Close()

		stats, err := cli.ContainerStats(context.Background(), id, client.ContainerStatsOptions{Stream: true})
		if err != nil {
			return
		}
		defer stats.Body.Close()

		io.Copy(&wsBinaryWriter{ws}, stats.Body)
	})
}

// WebSocket書き込み用ラッパー
type wsBinaryWriter struct{ *websocket.Conn }

func (w *wsBinaryWriter) Write(p []byte) (int, error) {
	err := w.WriteMessage(websocket.BinaryMessage, p)
	return len(p), err
}
