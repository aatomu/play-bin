package webdav

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
	"github.com/play-bin/internal/vfs"
	"golang.org/x/net/webdav"
)

// MARK: Server
type Server struct {
	Config *config.LoadedConfig
}

// MARK: NewServer()
func NewServer(cfg *config.LoadedConfig) *Server {
	return &Server{
		Config: cfg,
	}
}

// MARK: Handler()
// WebDAVリクエストを処理するHTTPハンドラーを返す。
func (s *Server) Handler() http.Handler {
	webdavHandler := &webdav.Handler{
		FileSystem: &vfsWebdavAdapter{config: s.Config},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !os.IsNotExist(err) {
				logger.Logf("Internal", "WebDAV", "エラー: %v %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Basic認証のチェック
		username, password, ok := r.BasicAuth()
		cfg := s.Config.Get()
		user, userOk := cfg.Users[username]

		if !ok || !userOk || user.Password != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="play-bin WebDAV"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			logger.Logf("Client", "WebDAV", "ログイン失敗: user=%s, addr=%s", username, r.RemoteAddr)
			return
		}

		// ユーザー情報をコンテキストに埋め込み
		ctx := context.WithValue(r.Context(), "user", username)
		// /dav/ プレフィックスをトリムして VFS に渡す
		http.StripPrefix("/dav/", webdavHandler).ServeHTTP(w, r.WithContext(ctx))
	})
}

// MARK: vfsWebdavAdapter
// internal/vfs.Handler を webdav.FileSystem インターフェースに適合させるためのアダプター。
type vfsWebdavAdapter struct {
	config *config.LoadedConfig
}

func (a *vfsWebdavAdapter) getHandler(ctx context.Context) *vfs.Handler {
	username, _ := ctx.Value("user").(string)
	return &vfs.Handler{
		Username: username,
		Config:   a.config,
	}
}

func (a *vfsWebdavAdapter) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	h := a.getHandler(ctx)
	// 書き込み権限チェック
	if err := a.checkWritePerm(h, name); err != nil {
		return err
	}

	fullPath, err := h.MapPath(name)
	if err != nil {
		return err
	}
	logger.Logf("Client", "WebDAV", "ディレクトリ作成: user=%s, path=%s", h.Username, name)
	return os.MkdirAll(fullPath, perm)
}

func (a *vfsWebdavAdapter) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	h := a.getHandler(ctx)

	// 書き込みフラグが含まれる場合は権限チェック
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		if err := a.checkWritePerm(h, name); err != nil {
			return nil, err
		}
	}

	fullPath, err := h.MapPath(name)
	if err != nil {
		// ルートまたはコンテナルートの場合は仮想ディレクトリとして振る舞う
		if err == vfs.ErrVfsRoot {
			return &vfsWebdavFile{handler: h, isRoot: true}, nil
		}
		if err == vfs.ErrVfsContainerRoot {
			containerName := strings.Trim(name, "/")
			return &vfsWebdavFile{handler: h, containerName: containerName}, nil
		}
		return nil, err
	}

	f, err := os.OpenFile(fullPath, flag, perm)
	if err != nil {
		return nil, err
	}

	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		logger.Logf("Client", "WebDAV", "ファイル書込オープン: user=%s, path=%s", h.Username, name)
	}

	return f, nil
}

func (a *vfsWebdavAdapter) RemoveAll(ctx context.Context, name string) error {
	h := a.getHandler(ctx)
	if err := a.checkWritePerm(h, name); err != nil {
		return err
	}

	fullPath, err := h.MapPath(name)
	if err != nil {
		return err
	}
	logger.Logf("Client", "WebDAV", "削除操作: user=%s, path=%s", h.Username, name)
	return os.RemoveAll(fullPath)
}

func (a *vfsWebdavAdapter) Rename(ctx context.Context, oldName, newName string) error {
	h := a.getHandler(ctx)
	if err := a.checkWritePerm(h, oldName); err != nil {
		return err
	}
	if err := a.checkWritePerm(h, newName); err != nil {
		return err
	}

	oldPath, err := h.MapPath(oldName)
	if err != nil {
		return err
	}
	newPath, err := h.MapPath(newName)
	if err != nil {
		return err
	}

	logger.Logf("Client", "WebDAV", "リネーム: user=%s, %s -> %s", h.Username, oldName, newName)
	return os.Rename(oldPath, newPath)
}

func (a *vfsWebdavAdapter) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	h := a.getHandler(ctx)
	fullPath, err := h.MapPath(name)
	if err != nil {
		if err == vfs.ErrVfsRoot {
			return vfs.NewFileInfo("", true), nil
		}
		if err == vfs.ErrVfsContainerRoot {
			containerName := strings.Trim(name, "/")
			return vfs.NewFileInfo(containerName, true), nil
		}
		return nil, err
	}
	return os.Stat(fullPath)
}

func (a *vfsWebdavAdapter) checkWritePerm(h *vfs.Handler, path string) error {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return os.ErrPermission
	}
	containerName := parts[0]
	cfg := h.Config.Get()
	user := cfg.Users[h.Username]
	if !user.HasPermission(containerName, config.PermFileWrite) {
		return os.ErrPermission
	}
	return nil
}

// MARK: vfsWebdavFile
// 仮想ディレクトリ（ルートおよびコンテナルート）を webdav.File として扱うための実装。
type vfsWebdavFile struct {
	handler       *vfs.Handler
	isRoot        bool
	containerName string
	offset        int
}

func (f *vfsWebdavFile) Close() error                                 { return nil }
func (f *vfsWebdavFile) Read(p []byte) (int, error)                   { return 0, os.ErrInvalid }
func (f *vfsWebdavFile) Seek(offset int64, whence int) (int64, error) { return 0, os.ErrInvalid }
func (f *vfsWebdavFile) Write(p []byte) (int, error)                  { return 0, os.ErrPermission }

func (f *vfsWebdavFile) Stat() (os.FileInfo, error) {
	if f.isRoot {
		return vfs.NewFileInfo("", true), nil
	}
	return vfs.NewFileInfo(f.containerName, true), nil
}

func (f *vfsWebdavFile) Readdir(count int) ([]os.FileInfo, error) {
	var items []os.FileInfo
	cfg := f.handler.Config.Get()
	user := cfg.Users[f.handler.Username]

	if f.isRoot {
		for name := range cfg.Servers {
			if user.HasPermission(name, config.PermContainerRead) {
				items = append(items, vfs.NewFileInfo(name, true))
			}
		}
	} else {
		// コンテナルート：マウントポイント一覧
		// 注意: Readdir 内で docker client 呼び出しが必要
		inspect, err := docker.Client.ContainerInspect(context.Background(), f.containerName)
		if err == nil {
			for _, m := range inspect.Mounts {
				name := strings.Trim(m.Destination, "/")
				items = append(items, vfs.NewFileInfo(name, true))
			}
		}
	}

	// 簡易的なオフセット処理
	if f.offset >= len(items) {
		return nil, nil // io.EOF ではなく nil で終了を示す
	}

	start := f.offset
	end := len(items)
	if count > 0 && start+count < end {
		end = start + count
	}

	f.offset = end
	return items[start:end], nil
}
