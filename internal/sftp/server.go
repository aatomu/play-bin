package sftp

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/logger"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager
	sshConfig        *ssh.ServerConfig
}

// MARK: NewServer()
// SFTPサーバーの新しいインスタンスを生成し、SSH設定を初期化する。
func NewServer(cfg *config.LoadedConfig, cm *container.Manager) *Server {
	s := &Server{
		Config:           cfg,
		ContainerManager: cm,
	}

	sshConfig := &ssh.ServerConfig{
		PasswordCallback: s.authenticate,
	}

	// ホストの同一性を証明するためのキーを準備。存在しない場合は自動生成する。
	keyPath := "sftp_host_key"
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		logger.Log("Internal", "SFTP", "新規ホストキーを生成しています...")
		generateHostKey(keyPath)
	}

	keyBytes, err := os.ReadFile(keyPath)
	if err == nil {
		private, err := ssh.ParsePrivateKey(keyBytes)
		if err == nil {
			sshConfig.AddHostKey(private)
		} else {
			logger.Logf("Internal", "SFTP", "ホストキーのパース失敗: %v", err)
		}
	} else {
		logger.Logf("Internal", "SFTP", "ホストキーの読み込み失敗: %v", err)
	}

	s.sshConfig = sshConfig
	return s
}

// MARK: generateHostKey()
// SSH/SFTP接続に使用する暗号化キーを外部コマンドを用いて生成する。
func generateHostKey(path string) {
	cmd := exec.Command("ssh-keygen", "-f", path, "-N", "", "-t", "ed25519")
	if err := cmd.Run(); err != nil {
		logger.Logf("Internal", "SFTP", "ホストキーの生成に失敗しました: %v", err)
	}
}

// MARK: Start()
// 指定されたアドレスでTCPリスナーを開始し、SFTPのリクエスト待機を行う。
func (s *Server) Start() {
	listen := s.Config.Get().SFTPListen
	if listen == "" {
		// 設定が空の場合はセキュアな運用のためサーバーを無効化する
		logger.Log("Internal", "SFTP", "SFTPサーバーは無効です（sftpListenが未設定）")
		return
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Logf("Internal", "SFTP", "ポート %s のリスニング失敗: %v", listen, err)
		return
	}
	logger.Logf("Internal", "SFTP", "SFTPサーバーが開始されました: \"%s\"", listen)

	for {
		// 新しい接続を非同期（ゴルーチン）で処理することで、複数のユーザー同時アクセスに対応する
		nConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go s.handleConn(nConn)
	}
}

// MARK: authenticate()
// SSH接続時のユーザー認証を行う。成功時にはユーザー情報をセッションに付与する。
func (s *Server) authenticate(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	cfg := s.Config.Get()
	user, ok := cfg.Users[c.User()]
	if !ok || user.Password != string(pass) {
		logger.Logf("Client", "SFTP", "ログイン失敗: user=%s, addr=%s", c.User(), c.RemoteAddr())
		return nil, fmt.Errorf("authentication failed")
	}

	logger.Logf("Client", "SFTP", "ログイン成功: user=%s, addr=%s", c.User(), c.RemoteAddr())
	return &ssh.Permissions{
		// 以降のファイル操作でユーザー名を特定するために情報を保持
		Extensions: map[string]string{"user": c.User()},
	}, nil
}

// MARK: handleConn()
// 確立された通信経路に対して、SFTPサブシステムのハンドラを紐づける。
func (s *Server) handleConn(nConn net.Conn) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, s.sshConfig)
	if err != nil {
		return
	}
	defer sConn.Close()

	// SSHのその他のリクエスト（シェル等）は無視し、SFTPのみを許可する
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
				// SFTPサブシステムとしての要求のみを承諾する
				if req.Type == "subsystem" && string(req.Payload[4:]) == "sftp" {
					ok = true
				}
				req.Reply(ok, nil)
			}
		}(requests)

		// 仮想ファイルシステム用ハンドラの初期化
		username := sConn.Permissions.Extensions["user"]
		rootHandler := &vfsHandler{
			username: username,
			config:   s.Config,
		}

		// ハンドラを登録し、SFTPとしての命令解釈（読み・書き・一覧表示など）を開始
		server := sftp.NewRequestServer(channel, sftp.Handlers{
			FileGet:  rootHandler,
			FilePut:  rootHandler,
			FileCmd:  rootHandler,
			FileList: rootHandler,
		})

		if err := server.Serve(); err != nil && err != io.EOF {
			logger.Logf("Internal", "SFTP", "セッション異常終了 (user=%s): %v", username, err)
		}
	}
}

// MARK: vfsHandler
// ユーザーの権限に基づき、ホスト上のパスを仮想ディレクトリ構造としてマッピングする構造体。
type vfsHandler struct {
	username string
	config   *config.LoadedConfig
}

// MARK: mapPath()
// ユーザーからの抽象的なパスを、ホスト上の実パスへ変換・権限検証する。
func (h *vfsHandler) mapPath(path string) (string, error) {
	path = filepath.Clean(path)
	parts := strings.Split(strings.Trim(path, "/"), string(filepath.Separator))

	// ルート（全コンテナ一覧）の要求
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return "", logger.InternalError("SFTP", "root access")
	}

	containerName := parts[0]
	cfg := h.config.Get()
	user, userOk := cfg.Users[h.username]
	if !userOk {
		return "", os.ErrPermission
	}

	// 認可チェック: ユーザーがこのコンテナへの操作権限を持っているか
	allowed := false
	for _, p := range user.Controllable {
		if p == "*" || p == containerName {
			allowed = true
			break
		}
	}
	if !allowed {
		logger.Logf("Client", "SFTP", "アクセス拒否: user=%s, path=%s", h.username, path)
		return "", os.ErrPermission
	}

	server, ok := cfg.Servers[containerName]
	if !ok {
		return "", os.ErrNotExist
	}

	// コンテナのルート要求
	if len(parts) == 1 {
		return "", logger.InternalError("SFTP", "container_root access")
	}

	// マウントされたディレクトリの解決
	// 例: /server1/data/config.yml -> ホスト上の実パスへ変換
	targetSubPath := parts[1]
	for hostPath, containerPath := range server.Mount {
		cPath := strings.Trim(containerPath, "/")
		if cPath == targetSubPath {
			// マウントポイント配下の相対パスを結合して実パスを生成
			rel, _ := filepath.Rel(targetSubPath, strings.Join(parts[1:], "/"))
			return filepath.Join(hostPath, rel), nil
		}
	}

	return "", os.ErrNotExist
}

// MARK: Filelist()
// ディレクトリ内のファイル一覧を解決する。
func (h *vfsHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		// ルート階層では、ユーザーがアクセス可能なコンテナ名のリストを仮想的に生成する
		if err.Error() == "[SFTP] root access" {
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
		// コンテナ直下では、マウントされているポイントを仮想ディレクトリとして返す
		if err.Error() == "[SFTP] container_root access" {
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

	// 実パス配下は通常のファイルシステムとして読み取る
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

// MARK: Fileread()
// ファイルの読み込みリクエストを処理する。
func (h *vfsHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	logger.Logf("Client", "SFTP", "ファイル読込: user=%s, path=%s", h.username, r.Filepath)
	return os.Open(fullPath)
}

// MARK: Filewrite()
// ファイルへの書き込みリクエストを処理する。
func (h *vfsHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	logger.Logf("Client", "SFTP", "ファイル書込: user=%s, path=%s", h.username, r.Filepath)
	// 常に新規作成、または上書きとしてオープンする。
	return os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
}

// MARK: Filecmd()
// ファイルの削除、改名、ディレクトリ作成等の各種操作を実行する。
func (h *vfsHandler) Filecmd(r *sftp.Request) error {
	fullPath, err := h.mapPath(r.Filepath)
	if err != nil {
		return err
	}

	// 操作の種類に応じてログを出力
	logger.Logf("Client", "SFTP", "操作実行 (%s): user=%s, path=%s", r.Method, h.username, r.Filepath)

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
		return logger.ClientError("SFTP", "シンボリックリンクはサポートされていません")
	}
	return nil
}

// MARK: listerAt
// sftp.ListerAt インターフェースを満たし、一覧のオフセット処理をサポートする構造体。
type listerAt struct {
	items []os.FileInfo
}

// MARK: ListAt()
// 指定されたオフセットからファイル情報をコピーして返す。
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

// MARK: vfsFileInfo
// 仮想ディレクトリ項目を os.FileInfo として見せるための擬似的な構造体。
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
