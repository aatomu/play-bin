package sftp

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager
	sshConfig        *ssh.ServerConfig
}

// MARK: NewServer()
func NewServer(cfg *config.LoadedConfig, cm *container.Manager) *Server {
	s := &Server{
		Config:           cfg,
		ContainerManager: cm,
	}

	sshConfig := &ssh.ServerConfig{
		PasswordCallback: s.authenticate,
	}

	// ホストキーの準備
	keyPath := "sftp_host_key"
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		log.Println("SFTP: Generating new host key...")
		generateHostKey(keyPath)
	}

	keyBytes, err := os.ReadFile(keyPath)
	if err == nil {
		private, err := ssh.ParsePrivateKey(keyBytes)
		if err == nil {
			sshConfig.AddHostKey(private)
		}
	}

	s.sshConfig = sshConfig
	return s
}

func generateHostKey(path string) {
	cmd := exec.Command("ssh-keygen", "-f", path, "-N", "", "-t", "ed25519")
	if err := cmd.Run(); err != nil {
		log.Printf("SFTP: Failed to generate host key: %v", err)
	}
}

// MARK: Start()
func (s *Server) Start() {
	listen := s.Config.Get().SFTPListen
	if listen == "" {
		log.Println("SFTP server is disabled (sftpListen is empty)")
		return
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		log.Printf("SFTP: Failed to listen on %s: %v", listen, err)
		return
	}
	log.Printf("SFTP server has started at \"%s\"", listen)

	for {
		nConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go s.handleConn(nConn)
	}
}

func (s *Server) authenticate(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	cfg := s.Config.Get()
	user, ok := cfg.Users[c.User()]
	if !ok || user.Password != string(pass) {
		return nil, fmt.Errorf("auth failed")
	}
	return &ssh.Permissions{
		Extensions: map[string]string{"user": c.User()},
	}, nil
}

func (s *Server) handleConn(nConn net.Conn) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, s.sshConfig)
	if err != nil {
		return
	}
	defer sConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unknown channel")
			continue
		}

		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}

		go func(in <-chan *ssh.Request) {
			for req := range in {
				ok := false
				if req.Type == "subsystem" && string(req.Payload[4:]) == "sftp" {
					ok = true
				}
				req.Reply(ok, nil)
			}
		}(requests)

		// 仮想ファイルシステム用ハンドラ
		username := sConn.Permissions.Extensions["user"]
		rootHandler := &vfsHandler{
			username: username,
			config:   s.Config,
		}

		server := sftp.NewRequestServer(channel, sftp.Handlers{
			FileGet:  rootHandler,
			FilePut:  rootHandler,
			FileCmd:  rootHandler,
			FileList: rootHandler,
		})

		if err := server.Serve(); err != nil && err != io.EOF {
			log.Printf("SFTP: Server error: %v", err)
		}
	}
}

// MARK: vfsHandler
// ユーザーの権限に基づいて、コンテナのマウントパスを仮想的にマッピングする
type vfsHandler struct {
	username string
	config   *config.LoadedConfig
}

func (h *vfsHandler) mapPath(path string) (string, error) {
	path = filepath.Clean(path)
	parts := strings.Split(strings.Trim(path, "/"), string(filepath.Separator))

	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return "", fmt.Errorf("root") // Root level request
	}

	containerName := parts[0]
	cfg := h.config.Get()
	user := cfg.Users[h.username]

	// 権限チェック
	allowed := false
	for _, p := range user.Controllable {
		if p == "*" || p == containerName {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", os.ErrPermission
	}

	server, ok := cfg.Servers[containerName]
	if !ok {
		return "", os.ErrNotExist
	}

	if len(parts) == 1 {
		return "", fmt.Errorf("container_root") // Container root level
	}

	// マウントパスの解決
	// 例: /container1/data/world -> host_path/world
	// mounts のキーがホストパス、値がコンテナパス
	// 仮想FS上では、便宜上 mounts の "ホストパスのベース名" をフォルダ名として見せるか、
	// あるいは単にマウント設定をそのまま見せる。
	// ここでは単純化のため、mounts の「キー（ホストパス）」の末尾ディレクトリ名を使う、
	// または設定の順番に基づいた仮想名を使う。

	// より実用的なのは、マウント設定の順番で "mount1", "mount2" とするのではなく、
	// 「コンテナ内のパス」をキーにして見せること。
	// mounts: { "/home/atomu/data": "/data" } -> SFTP: /container1/data/
	targetSubPath := parts[1]
	for hostPath, containerPath := range server.Mount {
		cPath := strings.Trim(containerPath, "/")
		if cPath == targetSubPath {
			rel, _ := filepath.Rel(targetSubPath, strings.Join(parts[1:], "/"))
			return filepath.Join(hostPath, rel), nil
		}
	}

	return "", os.ErrNotExist
}

// Handlers interface implementations for vfsHandler

func (h *vfsHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		if err.Error() == "root" {
			cfg := h.config.Get()
			user := cfg.Users[h.username]
			var items []os.FileInfo
			for name := range cfg.Servers {
				allowed := false
				for _, p := range user.Controllable {
					if p == "*" || p == name {
						allowed = true
						break
					}
				}
				if allowed {
					items = append(items, &vfsFileInfo{name: name, isDir: true})
				}
			}
			return &listerAt{items: items}, nil
		}
		if err.Error() == "container_root" {
			containerName := strings.Trim(r.Filepath, "/")
			cfg := h.config.Get()
			server := cfg.Servers[containerName]
			var items []os.FileInfo
			for _, containerPath := range server.Mount {
				name := strings.Trim(containerPath, "/")
				items = append(items, &vfsFileInfo{name: name, isDir: true})
			}
			return &listerAt{items: items}, nil
		}
		return nil, err
	}

	files, err := os.ReadDir(fullPath)
	if err != nil {
		return nil, err
	}
	var items []os.FileInfo
	for _, f := range files {
		info, _ := f.Info()
		items = append(items, info)
	}
	return &listerAt{items: items}, nil
}

func (h *vfsHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	return os.Open(fullPath)
}

func (h *vfsHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
}

func (h *vfsHandler) Filecmd(r *sftp.Request) error {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		return err
	}
	switch r.Method {
	case "Setstat":
		return nil
	case "Rename":
		targetPath, err := h.mapPath(r.Target)
		if err != nil {
			return err
		}
		return os.Rename(fullPath, targetPath)
	case "Rmdir":
		return os.RemoveAll(fullPath)
	case "Mkdir":
		return os.MkdirAll(fullPath, 0755)
	case "Remove":
		return os.Remove(fullPath)
	case "Symlink":
		return fmt.Errorf("symlink not supported")
	}
	return nil
}

// ListerAt implementation
type listerAt struct {
	items []os.FileInfo
}

func (l *listerAt) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l.items)) {
		return 0, io.EOF
	}
	n := copy(ls, l.items[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

// 仮想FileInfo
type vfsFileInfo struct {
	name  string
	isDir bool
}

func (f *vfsFileInfo) Name() string { return f.name }
func (f *vfsFileInfo) Size() int64  { return 0 }
func (f *vfsFileInfo) Mode() os.FileMode {
	if f.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (f *vfsFileInfo) ModTime() time.Time { return time.Now() }
func (f *vfsFileInfo) IsDir() bool        { return f.isDir }
func (f *vfsFileInfo) Sys() interface{}   { return nil }
