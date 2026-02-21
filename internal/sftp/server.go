package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/pkg/sftp"
	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
	"github.com/play-bin/internal/vfs"
	"golang.org/x/crypto/ssh"
)

type Server struct {
	Config           *config.LoadedConfig
	ContainerManager *container.Manager
	sshConfig        *ssh.ServerConfig
}

// MARK: NewServer()
// SFTP サーバーのインスタンスを作成し、SSH 層の基礎設定（認証コールバックやホストキー）を行う。
func NewServer(cfg *config.LoadedConfig, cm *container.Manager) *Server {
	s := &Server{
		Config:           cfg,
		ContainerManager: cm,
	}

	sshConfig := &ssh.ServerConfig{
		// 接続時の認証処理。ここでの成否が SFTP セッションの可否に直結する。
		PasswordCallback: s.authenticate,
	}

	// サーバーの正当性を証明するホストキーの準備。
	keyPath := "sftp_host_key"
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		// 未生成の場合は、信頼性を確保するため初回起動時に自動生成を試みる。
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
// SSH 通信の暗号化に不可欠な ed25519 形式のキーペアを外部ユーティリティを用いて生成する。
func generateHostKey(path string) {
	cmd := exec.Command("ssh-keygen", "-f", path, "-N", "", "-t", "ed25519")
	if err := cmd.Run(); err != nil {
		// 生成失敗はシステム設定（パッケージ不足等）に起因するため Internal で記録。
		logger.Logf("Internal", "SFTP", "ホストキーの生成に失敗しました: %v", err)
	}
}

// MARK: Start()
// 設定されたアドレスで TCP ポートを開放し、リモートからの SFTP クライアント接続を待ち受ける。
func (s *Server) Start() {
	listen := s.Config.Get().SFTPListen
	if listen == "" {
		// リスニング設定が未定義の場合、誤って全ポートを公開するリスクを避けるため無効化する。
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
		// ユーザーごとの独立したセッションを確保するため、 Accept した接続はゴルーチンへ逃がす。
		nConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go s.handleConn(nConn)
	}
}

// MARK: authenticate()
// config.json に定義されたユーザー・パスワード情報を元に、SSH レベルの認証を行う。
func (s *Server) authenticate(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	cfg := s.Config.Get()
	user, ok := cfg.Users[c.User()]
	if !ok || user.Password != string(pass) {
		// 認証失敗は外部からのアタックの可能性があるため、発信元を含めて Client コンテキストで記録。
		logger.Logf("Client", "SFTP", "ログイン失敗: user=%s, addr=%s", c.User(), c.RemoteAddr())
		return nil, fmt.Errorf("authentication failed")
	}

	logger.Logf("Client", "SFTP", "ログイン成功: user=%s, addr=%s", c.User(), c.RemoteAddr())
	return &ssh.Permissions{
		// 認証後のファイル操作で、どのユーザーの権限を適用すべきか識別するためのメタデータを埋め込む。
		Extensions: map[string]string{"user": c.User()},
	}, nil
}

// MARK: handleConn()
// SSH ハンドシェイクが完了した接続に対し、SFTP プロトコルハンドラをアタッチしてファイル操作を開始可能にする。
func (s *Server) handleConn(nConn net.Conn) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, s.sshConfig)
	if err != nil {
		return
	}
	defer sConn.Close()

	// 誤用防止のため、SFTP 以外の SSH リクエスト（シェルアクセス等）は全て破棄する。
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
				// SFTP 専用のサブシステム要求のみを受理する。
				if req.Type == "subsystem" && string(req.Payload[4:]) == "sftp" {
					ok = true
				}
				req.Reply(ok, nil)
			}
		}(requests)

		// 仮想ファイルシステム（VFS）ハンドラの構築。ホストの直接アクセスは許可せず、マウント点のみを見せる。
		username := sConn.Permissions.Extensions["user"]
		rootHandler := &sftpHandler{
			handler: &vfs.Handler{
				Username: username,
				Config:   s.Config,
			},
		}

		// SFTP リクエスト（開く、読む、書く、消すなど）を VFS ハンドラへマッピングする。
		server := sftp.NewRequestServer(channel, sftp.Handlers{
			FileGet:  rootHandler,
			FilePut:  rootHandler,
			FileCmd:  rootHandler,
			FileList: rootHandler,
		})

		if err := server.Serve(); err != nil && !errors.Is(err, io.EOF) {
			logger.Logf("Internal", "SFTP", "セッション異常終了 (user=%s): %v", username, err)
		}
	}
}

// MARK: sftpHandler
// internal/vfs.Handler を SFTP プロトコルに適合させるためのアダプター。
type sftpHandler struct {
	handler *vfs.Handler
}

// MARK: Filelist()
// ディレクトリ内のファイル・フォルダ一覧を生成する。
func (h *sftpHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	fullPath, err := h.handler.MapPath(r.Filepath)

	// セキュリティ監査のため、ディレクトリ一覧の取得は常に記録する。
	logger.Logf("Client", "SFTP", "ディレクトリ一覧取得: user=%s, path=%s", h.handler.Username, r.Filepath)

	if err != nil {
		// ルート階層：許可されたコンテナ名を一覧として返す。
		if err == vfs.ErrVfsRoot {
			cfg := h.handler.Config.Get()
			user := cfg.Users[h.handler.Username]
			var items []os.FileInfo
			for name := range cfg.Servers {
				if user.HasPermission(name, config.PermContainerRead) {
					items = append(items, vfs.NewFileInfo(name, true))
				}
			}
			return &listerAt{items: items}, nil
		}
		// コンテナ直下：設定されたマウントポイント名を一覧として返す。
		if err == vfs.ErrVfsContainerRoot {
			containerName := strings.Trim(r.Filepath, "/")
			var items []os.FileInfo

			// コンテナの実体から現在のマウント状況を問い合わせる。
			inspect, err := docker.Client.ContainerInspect(context.Background(), containerName)
			if err == nil {
				for _, m := range inspect.Mounts {
					name := strings.Trim(m.Destination, "/")
					items = append(items, vfs.NewFileInfo(name, true))
				}
			} else {
				logger.Logf("Internal", "SFTP", "コンテナ %s のマウント一覧取得失敗: %v", containerName, err)
			}
			return &listerAt{items: items}, nil
		}
		return nil, err
	}

	// 実ホスト上のディレクトリ読み取り。
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
// 物理ファイルの中身を取り出す。
func (h *sftpHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	fullPath, err := h.handler.MapPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	// トレーサビリティのため、ダウンロード操作を記録。
	logger.Logf("Client", "SFTP", "ファイル読込: user=%s, path=%s", h.handler.Username, r.Filepath)
	return os.Open(fullPath)
}

// MARK: Filewrite()
// 物理ファイルへデータを上書き・追記する。
func (h *sftpHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	// 書き込み操作には write 権限が必要
	containerName := strings.Split(strings.Trim(r.Filepath, "/"), "/")[0]
	cfg := h.handler.Config.Get()
	user := cfg.Users[h.handler.Username]
	if !user.HasPermission(containerName, config.PermFileWrite) {
		return nil, os.ErrPermission
	}

	fullPath, err := h.handler.MapPath(r.Filepath)
	if err != nil {
		return nil, err
	}
	// データの変更を伴う操作のため、確実にログへ残す。
	logger.Logf("Client", "SFTP", "ファイル書込: user=%s, path=%s", h.handler.Username, r.Filepath)
	// 常に新規作成、または既存の内容を破棄して書き込むモードで開く。
	return os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
}

// MARK: Filecmd()
// ファイルの削除、フォルダ作成、名前変更等の「構成変更」コマンドを処理する。
func (h *sftpHandler) Filecmd(r *sftp.Request) error {
	// 変更操作には write 権限が必要
	containerName := strings.Split(strings.Trim(r.Filepath, "/"), "/")[0]
	cfg := h.handler.Config.Get()
	user := cfg.Users[h.handler.Username]
	if !user.HasPermission(containerName, config.PermFileWrite) {
		return os.ErrPermission
	}

	fullPath, err := h.handler.MapPath(r.Filepath)
	if err != nil {
		return err
	}

	// 管理者によるファイル削除等は事後の不備確認に不可欠なため、メソッド名を含めて記録。
	logger.Logf("Client", "SFTP", "構成変更操作 (%s): user=%s, path=%s", r.Method, h.handler.Username, r.Filepath)

	switch r.Method {
	case "Setstat":
		// パーミッション等の微調整は、環境の整合性担保のため一律無視（または成功扱い）とする。
		return nil
	case "Rename":
		// 移動先パス解決
		targetPath, err := h.handler.MapPath(r.Target)
		if err != nil {
			return err
		}
		logger.Logf("Client", "SFTP", "リネーム対象: %s -> %s (user=%s)", r.Filepath, r.Target, h.handler.Username)
		return os.Rename(fullPath, targetPath)
	case "Rmdir":
		return os.RemoveAll(fullPath)
	case "Mkdir":
		return os.MkdirAll(fullPath, 0755)
	case "Remove":
		return os.Remove(fullPath)
	case "Symlink":
		return logger.ClientError("SFTP", "シンボリックリンクの作成は許可されていません")
	}
	return nil
}

// MARK: listerAt
// 指定された範囲（オフセット）のファイル一覧データを切り出すためのヘルパー。
type listerAt struct {
	items []os.FileInfo
}

// MARK: ListAt()
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
