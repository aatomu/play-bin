package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	ctypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gorilla/websocket"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
)

var wsUpgrader = websocket.Upgrader{
	// 開発の簡便性と、Authミドルウェアによる事前のトークン検証を前提として、全てのOriginを許可する。
	CheckOrigin: func(r *http.Request) bool { return true },
}

// MARK: TerminalHandler()
// WebSocketを介してコンテナの標準入出力（Terminal/Logs）へのストリーミング接続を提供する。
func (s *Server) TerminalHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, mode := q.Get("id"), q.Get("mode")
		ctx := r.Context()
		var stream io.ReadWriteCloser
		var isTty bool

		// コンテナの設定を確認し、TTYが有効かどうかで出力のデマルチプレクス処理を切り替える。
		inspect, err := docker.Client.ContainerInspect(ctx, id)
		if err == nil {
			isTty = inspect.Config.Tty
		}

		// WebSocketハンドラーはAuthミドルウェアを経由しているが、Usernameは引き継がれないためトークンから再取得する。
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		s.WebSessionMu.RLock()
		username := s.WebSessions[token]
		s.WebSessionMu.RUnlock()
		// ユーザーが存在しない場合（Auth通過後にセッション切れ等）はAuth側で弾かれるはずだが念のため
		if username == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		user := s.Config.Get().Users[username]

		// 指定されたモードに応じて、適切なDockerストリームを初期化する。
		switch mode {
		case "exec":
			if !user.HasPermission(id, config.PermContainerWrite) {
				logger.Logf("Client", "API", "WS Exec拒否: user=%s, target=%s", username, id)
				http.Error(w, "Write permission required", http.StatusForbidden)
				return
			}
			// インタラクティブなシェル操作を提供するため、TTYを強制しつつ環境変数を最適化する。
			isTty = true
			cfg := ctypes.ExecOptions{
				Tty: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
				Env: []string{"TERM=xterm-256color"}, Cmd: []string{"/bin/sh"},
			}
			cExec, err := docker.Client.ContainerExecCreate(ctx, id, cfg)
			if err != nil {
				logger.Logf("Internal", "API", "Exec作成失敗: container=%s, err=%v", id, err)
				return
			}
			resp, err := docker.Client.ContainerExecAttach(ctx, cExec.ID, ctypes.ExecAttachOptions{Tty: true})
			if err != nil {
				logger.Logf("Internal", "API", "Execアタッチ失敗: container=%s, err=%v", id, err)
				return
			}
			stream = resp.Conn
			logger.Logf("Internal", "API", "Exec接続を開始しました: container=%s", id)

		case "logs":
			if !user.HasPermission(id, config.PermContainerRead) {
				logger.Logf("Client", "API", "WS Logs拒否: user=%s, target=%s", username, id)
				http.Error(w, "Read permission required", http.StatusForbidden)
				return
			}
			// コンテナの開始時からのログを、指定された行数（tail）分取得してストリームを開始する。
			// 初期表示や無限スクロール時の重複読み込みを防ぐためのパラメータ。
			tail := q.Get("tail")
			if tail == "" {
				tail = "all"
			}
			logOptions := ctypes.LogsOptions{
				ShowStdout: true, ShowStderr: true, Follow: true, Tail: tail,
			}
			logs, err := docker.Client.ContainerLogs(ctx, id, logOptions)
			if err != nil {
				logger.Logf("Internal", "API", "ログ取得失敗: container=%s, err=%v", id, err)
				http.Error(w, "Failed to get logs", http.StatusInternalServerError)
				return
			}
			// ログモードでは入力（stdin）を送る必要がないため、書き込みを無視するラッパーを使用する。
			stream = &docker.ReadNullWriteCloser{R: logs}
			logger.Logf("Internal", "API", "ログストリーミングを開始しました: container=%s, tail=%s", id, tail)
		}

		if stream != nil {
			defer stream.Close()
		}

		// HTTP接続をWebSocketにアップグレードし、双方向通信を確立する。
		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Logf("Internal", "API", "WebSocketアップグレード失敗: %v", err)
			return
		}
		defer ws.Close()

		var once sync.Once
		done := make(chan struct{})
		cleanup := func() {
			// いずれかの通信路が切れた際に、全ての関連リソースを一括でクリーンアップしメモリリークを防ぐ。
			once.Do(func() {
				close(done)
				if stream != nil {
					stream.Close()
				}
				ws.Close()
				logger.Logf("Internal", "API", "WebSocket接続が切断されました: container=%s, mode=%s", id, mode)
			})
		}

		// MARK: > Docker to WebSocket
		// コンテナからの標準出力を捕捉し、WebSocketクライアントへと転送する。
		go func() {
			defer cleanup()
			wsWriter := &wsBinaryWriter{ws}
			if isTty {
				// TTYが有効な場合はそのまま転送可能。
				io.Copy(wsWriter, stream)
			} else {
				// TTYなし（ログ等）の場合は、docker特有のヘッダー（stdout/stderr識別用）を除去して転送する。
				stdcopy.StdCopy(wsWriter, wsWriter, stream)
			}
		}()

		// MARK: > WebSocket to Docker
		// クライアントからの入力を捕捉し、コンテナの標準入力へと流し込む（主にExecモード用）。
		go func() {
			defer cleanup()
			for {
				_, msg, err := ws.ReadMessage()
				if err != nil {
					// クライアント側からの切断やエラーを検知して終了する。
					return
				}
				stream.Write(msg)
			}
		}()

		<-done
	}
}

// MARK: StatsHandler()
// WebSocketを介してコンテナの統計情報（CPU/Memory/Network）をリアルタイムに配信する。
func (s *Server) StatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")

		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		s.WebSessionMu.RLock()
		username := s.WebSessions[token]
		s.WebSessionMu.RUnlock()

		user := s.Config.Get().Users[username]
		if !user.HasPermission(id, config.PermContainerRead) {
			// 統計情報の取得はRead権限が必要
			http.Error(w, "Read permission required", http.StatusForbidden)
			return
		}

		ws, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			logger.Logf("Internal", "API", "Stats WebSocketアップグレード失敗: %v", err)
			return
		}
		defer ws.Close()

		// Docker SDKからストリーム形式で統計情報を取得し続け、OS全体の情報を付与してWebSocketへ流し込む。
		stats, err := docker.Client.ContainerStats(r.Context(), id, true)
		if err != nil {
			logger.Logf("Internal", "API", "統計情報取得失敗: container=%s, err=%v", id, err)
			return
		}
		defer stats.Body.Close()

		decoder := json.NewDecoder(stats.Body)
		for {
			var dockerStats map[string]any
			if err := decoder.Decode(&dockerStats); err != nil {
				if err == io.EOF {
					break
				}
				logger.Logf("Internal", "API", "Docker統計デコード失敗: %v", err)
				break
			}

			// OS全体の情報を取得 (サンプリング間隔を持たせて安定させる)
			v, _ := mem.VirtualMemory()
			c, _ := cpu.Percent(200*time.Millisecond, false)

			// 情報を付与
			osStats := map[string]any{
				"memory_used_percent": v.UsedPercent,
				"memory_total":        v.Total,
				"memory_used":         v.Total - v.Available, // htop 等に近い「直感的な」使用量 (Total - Available)
				"cpu_percent":         0.0,
			}
			if len(c) > 0 {
				osStats["cpu_percent"] = c[0]
			}
			dockerStats["os_stats"] = osStats

			if err := ws.WriteJSON(dockerStats); err != nil {
				break
			}
		}
	}
}
