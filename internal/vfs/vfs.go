package vfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

var (
	// VFS 内の特殊な階層（コンテナ一覧、マウント一覧）を識別するための内部エラー定数。
	ErrVfsRoot          = fmt.Errorf("vfs_root")
	ErrVfsContainerRoot = fmt.Errorf("vfs_container_root")
)

// MARK: Handler
// ホスト上の実ディレクトリを秘匿し、ユーザーにはコンテナ名とマウント先のみをディレクトリとして提示する。
type Handler struct {
	Username string
	Config   *config.LoadedConfig
}

// MARK: MapPath()
// ユーザーが指定した仮想パスを、ホスト上の物理パスに厳密に解決・バリデートする。
func (h *Handler) MapPath(path string) (string, error) {
	path = filepath.Clean(path)
	parts := strings.Split(strings.Trim(path, "/"), string(filepath.Separator))

	// ルート階層（全ての「コンテナ名」が並ぶ階層）の要求。
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return "", ErrVfsRoot
	}

	containerName := parts[0]
	cfg := h.Config.Get()
	user, userOk := cfg.Users[h.Username]
	if !userOk {
		return "", os.ErrPermission
	}

	// 指定されたコンテナに対し、このユーザーが操作を許可されているか（read権限）を確認。
	if !user.HasPermission(containerName, config.PermContainerRead) {
		logger.Logf("Client", "VFS", "アクセス拒否: user=%s, path=%s", h.Username, path)
		return "", os.ErrPermission
	}

	if _, ok := cfg.Servers[containerName]; !ok {
		return "", os.ErrNotExist
	}

	// コンテナの中（マウントポイント一覧）の要求。
	if len(parts) == 1 {
		return "", ErrVfsContainerRoot
	}

	targetSubPath := parts[1]

	// コンテナの実体からマウント情報を動的に取得する。
	inspect, err := docker.Client.ContainerInspect(context.Background(), containerName)
	if err != nil {
		logger.Logf("Internal", "VFS", "コンテナ %s の詳細取得失敗: %v", containerName, err)
		return "", os.ErrNotExist
	}

	for _, m := range inspect.Mounts {
		cPath := strings.Trim(m.Destination, "/")
		if cPath == targetSubPath {
			// マウントポイントより下位の相対パスを抽出し、ホスト上の実パスと結合する。
			rel, _ := filepath.Rel(targetSubPath, strings.Join(parts[1:], "/"))
			return filepath.Join(m.Source, rel), nil
		}
	}

	return "", os.ErrNotExist
}

// MARK: FileInfo
// 物理的なファイルが存在しない仮想階層（コンテナ名など）を表現するための FileInfo 実装。
type FileInfo struct {
	name  string
	isDir bool
}

func (f *FileInfo) Name() string { return f.name }
func (f *FileInfo) Size() int64  { return 0 }
func (f *FileInfo) Mode() os.FileMode {
	if f.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (f *FileInfo) ModTime() time.Time { return time.Now() }
func (f *FileInfo) IsDir() bool        { return f.isDir }
func (f *FileInfo) Sys() any           { return nil }

// MARK: NewFileInfo()
func NewFileInfo(name string, isDir bool) os.FileInfo {
	return &FileInfo{name: name, isDir: isDir}
}
