package docker

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var (
	Client *client.Client

	// AttachLocks prevents multiple concurrent attaches to the same container
	AttachLocks  = make(map[string]bool)
	AttachLockMu sync.Mutex
)

// MARK: Init()
func Init() error {
	var err error
	Client, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	return err
}

// MARK: SendCommand()
// コンテナのstdinにコマンドを送信する
func SendCommand(id, command string) error {
	ctx := context.Background()

	resp, err := Client.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true,
		Stdin:  true,
	})
	if err != nil {
		return err
	}
	defer resp.Close()

	_, err = resp.Conn.Write([]byte(command))
	return err
}

// MARK: SendExec()
// コンテナでコマンドを実行する
func SendExec(id string, cmd []string) error {
	ctx := context.Background()
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	resp, err := Client.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return err
	}

	attach, err := Client.ContainerExecAttach(ctx, resp.ID, container.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer attach.Close()

	// 完了待ち
	io.Copy(io.Discard, attach.Reader)

	// 終了コード確認
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
