package docker

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/play-bin/internal/logger"
)

var (
	// Client は外部から参照可能な Docker SDK クライアントの共通インスタンス。
	Client *client.Client
)

// MARK: Init()
// OS 環境変数等を読み込み、Docker デーモンとの通信に必要なクライアントを初期化する。
func Init() error {
	var err error
	// API バージョンのネゴシエーションを有効にし、ホスト側の Docker 環境に自動で適応させる。
	Client, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		// 初期化失敗は主に Docker デーモン未起動などの外部要因（External）として記録する。
		logger.Logf("External", "Docker", "クライアント初期化失敗: %v", err)
	}
	return err
}

// MARK: SendCommand()
// 実行中のコンテナの標準入力 (stdin) へ、文字列（コマンド）を直接流し込む。
func SendCommand(id, command string) error {
	ctx := context.Background()

	// ストリーム接続（Attach）を確立する。TTY 有効なコンテナへのコマンド送信に利用。
	resp, err := Client.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true,
		Stdin:  true,
	})
	if err != nil {
		return err
	}
	defer resp.Close()

	// アプリケーションが解釈可能な生バイト列を stdin へ書き込む。
	_, err = resp.Conn.Write([]byte(command))
	return err
}

// MARK: SendExec()
// コンテナ内に一時的な別プロセスを生成（Exec）し、指定された引数リストでコマンドを同期実行する。
func SendExec(id string, cmd []string) error {
	ctx := context.Background()
	// コマンドの実行環境（出力のキャプチャ等）を定義する。
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	// Docker エンジンに対して、コマンド実行ジョブの作成を依頼する。
	resp, err := Client.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return err
	}

	// 作成したジョブに対してアタッチし、実際の実行を開始させる。
	attach, err := Client.ContainerExecAttach(ctx, resp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer attach.Close()

	// 同期実行を維持するため、プロセスが終了して出力ストリームが閉じるまで、出力を読み飛ばして待機する。
	io.Copy(io.Discard, attach.Reader)

	// プロセスの正常終了（終了コード 0）を担保するため、実行後の状態を詳細に検証する。
	inspect, err := Client.ContainerExecInspect(ctx, resp.ID)
	if err != nil {
		return err
	}
	if inspect.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d: %w", inspect.ExitCode, errors.New("non-zero exit code"))
	}

	return nil
}

// MARK: ReadNullWriteCloser
// データの読み込みのみに興味があり、書き込み操作を透過的に捨てたい場合に使用する io.ReadWriteCloser 実装。
type ReadNullWriteCloser struct {
	R io.ReadCloser
}

func (n *ReadNullWriteCloser) Write(b []byte) (int, error) {
	// 全ての書き込み内容を破棄する。
	return io.Discard.Write(b)
}
func (n *ReadNullWriteCloser) Read(b []byte) (int, error) {
	// 元のリーダーからデータを読み出す。
	return n.R.Read(b)
}
func (n *ReadNullWriteCloser) Close() error {
	// 下位のリーダーを適切に解放する。
	return n.R.Close()
}
