package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

// MARK: var
var (
	upgrader = websocket.Upgrader{
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

	// MARK: Http server
	mux := http.NewServeMux()
	// MARK: > File server
	mux.Handle("/", http.FileServer(http.Dir("./")))
	// MARK: > API server
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/containers", auth(func(w http.ResponseWriter, r *http.Request) {
		containers, err := cli.ContainerList(r.Context(), container.ListOptions{All: true})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		sessionLock.RLock()
		username := sessions[token]
		sessionLock.RUnlock()

		cfg := getConfig()
		user := cfg.Users[username]

		// フィルタリング
		filtered := []container.Summary{}
		for _, c := range containers {
			isAllowed := false
			// 最初の1つをチェック
			if len(c.Names) > 0 {
				// 先頭の "/" を消す
				name := c.Names[0][1:]

				for _, pattern := range user.Controllable {
					if pattern == "*" || pattern == name {
						isAllowed = true
						break
					}
				}
			}

			if isAllowed {
				filtered = append(filtered, c)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(filtered)
	}))
	mux.HandleFunc("/api/container/inspect", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		inspect, err := cli.ContainerInspect(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(inspect)
	}))
	// MARK: > Websocket
	mux.HandleFunc("/ws/terminal", auth(handleTerminal()))
	mux.HandleFunc("/ws/stats", auth(handleStats()))

	listen := getConfig().Listen
	log.Printf("Console Server started at \"%s\"", listen)
	log.Fatal(http.ListenAndServe(listen, middleware(mux)))
}

// MARK: Config
type loadedConfig struct {
	Config
	lastLoaded time.Time
	mu         sync.RWMutex
}
type Config struct {
	Listen string                `json:"listen"`
	Users  map[string]ConfigUser `json:"users"`
}

type ConfigUser struct {
	Password     string   `json:"password"`
	Controllable []string `json:"controllable"`
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
		log.Printf("Config読み込み失敗: %v", err)
		return
	}
	defer f.Close()

	var newCfg Config
	if err := json.NewDecoder(f).Decode(&newCfg); err != nil {
		log.Printf("Configパース失敗: %v", err)
		return
	}

	lc.Config = newCfg
	lc.lastLoaded = modTime
	log.Println("Configを自動リロードしました")
}

// MARK: Access log
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

func (lrw *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := lrw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("ResponseWriter does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

func middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// ステータスコードのキャプチャ用ラッパー
		lrw := &loggingResponseWriter{w, http.StatusOK}

		next.ServeHTTP(lrw, r)

		log.Printf("%s %s %s %d %v %s",
			r.Method,
			r.URL.Path,
			r.RemoteAddr,
			lrw.statusCode,
			time.Since(start),
			r.UserAgent(),
		)
	})
}

// MARK: Websocket()
type wsBinaryWriter struct{ *websocket.Conn }

func (w *wsBinaryWriter) Write(p []byte) (int, error) {
	if w.Conn == nil {
		return 0, os.ErrInvalid // または適当なエラー
	}
	err := w.WriteMessage(websocket.BinaryMessage, p)
	return len(p), err
}

// MARK: handleLogin()
func handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// ユーザー確認
	cfg := getConfig()
	user, ok := cfg.Users[creds.Username]
	if !ok || user.Password != creds.Password {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// トークン生成
	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)

	// セッション保存 (ユーザーIDを紐付け)
	sessionLock.Lock()
	sessions[token] = creds.Username
	sessionLock.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

// MARK: auth()
func auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// セッションチェック
		token := r.Header.Get("Authorization")
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		sessionLock.RLock()
		username, ok := sessions[token]
		sessionLock.RUnlock()

		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// 権限チェック
		containerID := r.URL.Query().Get("id")
		if containerID != "" {
			cfg := getConfig()
			user, exists := cfg.Users[username]
			if !exists {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}

			// Dockerからコンテナの実際の名前を取得
			inspect, err := cli.ContainerInspect(r.Context(), containerID)
			if err != nil {
				http.Error(w, "Container Not Found", http.StatusNotFound)
				return
			}
			// 先頭の "/" を消す
			realName := inspect.Name[1:]

			allowed := false
			for _, pattern := range user.Controllable {
				if pattern == "*" || pattern == realName {
					allowed = true
					break
				}
			}

			if !allowed {
				log.Printf("Permission Denied: user=%s, target_name=%s", username, realName)
				http.Error(w, "Forbidden: No permission for this container name", http.StatusForbidden)
				return
			}
		}

		next(w, r)
	}
}
