package docker

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/play-bin/internal/logger"
)

var (
	Client *client.Client

	// AttachLocks は同じコンテナに対する複数の同時アタッチを防ぎ、ストリームの混線を防止する。
	AttachLocks  = make(map[string]bool)
	AttachLockMu sync.Mutex
)

// MARK: Init()
// Docker SDKのクライアントを初期化する。
func Init() error {
	var err error
	// 環境変数を読み込み、Dockerデーモンとの通信を確立する。
	Client, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Logf("External", "Docker", "クライアント初期化失敗: %v", err)
	}
	return err
}

// MARK: SendCommand()
// コンテナの標準入力(stdin)に対して生の文字列を送信する。
// 主に停止コマンドの送信や、インタラクティブでないコマンド実行に利用する。
func SendCommand(id, command string) error {
	ctx := context.Background()

	// ストリームを確立し、コマンドを書き込むためのレスポンスを得る
	resp, err := Client.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true,
		Stdin:  true,
	})
	if err != nil {
		return err
	}
	defer resp.Close()

	// 指定されたコマンド（文字列）をバイト配列として書き込む
	_, err = resp.Conn.Write([]byte(command))
	return err
}

// MARK: SendExec()
// コンテナ内で新しいプロセス（exec）を生成して実行する。
func SendExec(id string, cmd []string) error {
	ctx := context.Background()
	// コマンド実行時の各種オプション（出力の取得など）を定義
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	// Dockerエンジンに対して実行用インスタンスの作成を依頼
	resp, err := Client.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return err
	}

	// 作成したインスタンスにアタッチし、実行を開始
	attach, err := Client.ContainerExecAttach(ctx, resp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer attach.Close()

	// プロセスが終了するまで出力を読み飛ばして待機する（同期実行のため）
	io.Copy(io.Discard, attach.Reader)

	// 正確な終了状態を把握するため、事後にインスペクトを行う
	inspect, err := Client.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d", inspect.ExitCode)
	}

	return nil
}

// ReadNullWriteCloser is a helper for logs
type ReadNullWriteCloser struct {
	R io.ReadCloser
}

func (n *ReadNullWriteCloser) Write(b []byte) (int, error) {
	return io.Discard.Write(b)
}
func (n *ReadNullWriteCloser) Read(b []byte) (int, error) {
	return n.R.Read(b)
}
func (n *ReadNullWriteCloser) Close() error {
	return n.R.Close()
}
