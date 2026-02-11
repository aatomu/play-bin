package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	ctypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"github.com/play-bin/internal/config"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionKill    Action = "kill"
	ActionBackup  Action = "backup"
	ActionRestore Action = "restore"
)

// Manager handles high-level container operations
type Manager struct {
	Config *config.LoadedConfig
}

// MARK: ExecuteAction()
// 指定されたアクション（起動、停止など）をコンテナに対して実行する。
func (m *Manager) ExecuteAction(ctx context.Context, id string, action Action) error {
	// アクションの種類に応じて、低レベルな個別メソッドに処理を委譲する。
	switch action {
	case ActionStart:
		return m.Start(ctx, id)
	case ActionStop:
		return m.Stop(ctx, id)
	case ActionKill:
		return m.Kill(ctx, id)
	case ActionBackup:
		return m.Backup(ctx, id)
	case ActionRestore:
		return m.Restore(ctx, id)
	default:
		return fmt.Errorf("unknown action: %s", action)
	}
}

// MARK: Start()
// コンフィグ情報を元にコンテナを（必要なら再作成して）起動する。
func (m *Manager) Start(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok || server.Image == "" {
		// 設定が存在しない、またはイメージが未定義の場合は、起動対象外として何もしない。
		return nil
	}

	// コンテナのランタイム設定。TTYを有効にすることで、Web経由のターミナル操作を可能にする。
	containerConfig := &ctypes.Config{
		Image:     server.Image,
		Tty:       true,
		OpenStdin: true,
	}

	// カスタムの起動コマンドが指定されている場合のみ、エントリポイントや引数を上書きする。
	if server.Commands.Start != nil {
		if e := server.Commands.Start.Entrypoint; e != "" {
			containerConfig.Entrypoint = strings.Fields(e)
		}
		if a := server.Commands.Start.Arguments; a != "" {
			containerConfig.Cmd = strings.Fields(a)
		}
	}

	hostConfig := &ctypes.HostConfig{}

	// 設定された全ディレクトリをホストからコンテナのボリュームとしてマッピングする。
	for hostPath, containerPath := range server.Mount {
		hostConfig.Binds = append(hostConfig.Binds, hostPath+":"+containerPath)
	}

	// ネットワーク接続モードの決定。明示的な指定がない場合は、隔離性の高い bridge モードを採用する。
	hostConfig.NetworkMode = ctypes.NetworkMode(server.Network.Mode)
	if hostConfig.NetworkMode == "" {
		hostConfig.NetworkMode = "bridge"
	}

	// ブリッジモード使用時のみ、外部公開用のポートフォワーディングを動的に構築する。
	if hostConfig.NetworkMode == "bridge" && len(server.Network.Mapping) > 0 {
		portBindings := nat.PortMap{}
		exposedPorts := nat.PortSet{}
		for hostPort, containerPort := range server.Network.Mapping {
			port := nat.Port(containerPort + "/tcp")
			exposedPorts[port] = struct{}{}
			portBindings[port] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: hostPort}}
		}
		containerConfig.ExposedPorts = exposedPorts
		hostConfig.PortBindings = portBindings
	}

	// 設定の変更を反映させるため、既に同名のコンテナが存在する場合は一旦破棄する。
	// これにより、常に最新の config.json に基づいた環境が構築されることを保証する。
	if inspect, err := docker.Client.ContainerInspect(ctx, id); err == nil {
		if !inspect.State.Running {
			// 停止中の古いコンテナを削除し、再作成の準備を整える。
			_ = docker.Client.ContainerRemove(ctx, id, ctypes.RemoveOptions{})
		}
	} else if !client.IsErrNotFound(err) {
		logger.Logf("Internal", "Container", "コンテナ状態確認失敗(%s): %v", id, err)
		return err
	}

	// コンテナの実体を Docker エンジン上に生成する。
	if _, err := docker.Client.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, id); err != nil {
		logger.Logf("Internal", "Container", "コンテナ作成失敗(%s): %v", id, err)
		return err
	}

	// 生成したコンテナプロセスの実行を開始する。
	if err := docker.Client.ContainerStart(ctx, id, ctypes.StartOptions{}); err != nil {
		logger.Logf("Internal", "Container", "コンテナ起動失敗(%s): %v", id, err)
		return err
	}
	logger.Logf("Internal", "Container", "コンテナの起動に成功しました: %s", id)
	return nil
}

// MARK: Stop()
// カスタム停止シーケンス（ゲーム内コマンド送信等）を順守しつつ、コンテナを停止する。
func (m *Manager) Stop(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		// 管理対象外のコンテナは、標準的な停止命令（SIGTERM 等）のみを発行する。
		return docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{})
	}

	// データを安全に保存して終了させるため、Docker 停止前に定義済みのクリーンアップ手順を実行する。
	for _, cmd := range server.Commands.Stop {
		switch cmd.Type {
		case "attach":
			// コンテナの stdin に直接コマンドを流し込み、アプリケーションレベルの終了処理を促す。
			if err := docker.SendCommand(id, cmd.Arg); err != nil {
				logger.Logf("Internal", "Container", "%s: attachコマンド送信失敗: %v", id, err)
			}
		case "exec":
			// 外部から補助プロセスを実行してクリーンアップを行う。
			if err := docker.SendExec(id, []string{"/bin/sh", "-c", cmd.Arg}); err != nil {
				logger.Logf("Internal", "Container", "%s: exec実行失敗: %v", id, err)
			}
		case "log":
			// 運用の透明性を確保するため、重要なフェーズをシステムログに刻む。
			logger.Log("Internal", "Container", fmt.Sprintf("[%s] %s", id, cmd.Arg))
		case "sleep":
			// アプリケーションが完全にシャットダウンするまでの猶予期間を確保する。
			if dur, err := time.ParseDuration(cmd.Arg); err == nil {
				time.Sleep(dur)
			}
		}
	}

	// 全ての手順が完了、またはタイムアウト後に、Docker レベルでコンテナを最終停止させる。
	if err := docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{}); err != nil {
		logger.Logf("Internal", "Container", "コンテナ停止失敗(%s): %v", id, err)
		return err
	}
	logger.Logf("Internal", "Container", "コンテナの停止に成功しました: %s", id)
	return nil
}

// MARK: Kill()
// 応答不能になったコンテナを、SIGKILL 等を用いて強制的に停止する。
func (m *Manager) Kill(ctx context.Context, id string) error {
	timeout := 30
	// 可能な限りリソースを壊さないよう、まずは短いタイムアウト付きで標準的な停止を試みる。
	if err := docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{Timeout: &timeout}); err == nil {
		logger.Logf("Internal", "Container", "コンテナが正常に停止しました(Kill経由): %s", id)
		return nil
	}
	// 標準停止が失敗した場合、OS レベルでプロセスを強制終了させる。
	err := docker.Client.ContainerKill(ctx, id, "SIGKILL")
	if err == nil {
		logger.Logf("Internal", "Container", "コンテナを強制終了しました: %s", id)
	}
	return err
}

// MARK: Backup()
// 指定されたパスのデータを、rsync を用いてインクリメンタルにバックアップする。
func (m *Manager) Backup(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		return fmt.Errorf("server %s not found in config", id)
	}

	timestamp := time.Now().Format("20060102_150405")
	var hasError bool

	// 整合性のあるバックアップを取得するため、事前に「保存」コマンド等を送信する必要があるかを確認する。
	isRunning := false
	if inspect, err := docker.Client.ContainerInspect(ctx, id); err == nil && inspect.State.Running {
		isRunning = true
	}

	for _, cmd := range server.Commands.Backup {
		switch cmd.Type {
		case "attach":
			// コンテナが起動していない場合は、stdinへのコマンド送信は失敗するためスキップする。
			if !isRunning {
				logger.Logf("Internal", "Container", "%s: コンテナ停止中のためバックアップ準備コマンド(attach)をスキップします", id)
				continue
			}
			// ゲームサーバー等の「save-all」コマンドを想定し、ディスクへの同期を促す。
			if err := docker.SendCommand(id, cmd.Arg+"\n"); err != nil {
				logger.Logf("Internal", "Container", "%s: バックアップ準備コマンド送信失敗: %v", id, err)
			}
		case "sleep":
			// コンテナが停止中の場合は待機も不要なためスキップする（時短）。
			if !isRunning {
				continue
			}
			// ディスク同期が完了するまでの待機時間。
			if dur, err := time.ParseDuration(cmd.Arg); err == nil {
				time.Sleep(dur)
			}
		case "backup":
			// 実データのコピー。rsync の --link-dest を使用し、変更がないファイルはハードリンクにする。
			parts := strings.SplitN(cmd.Arg, ":", 2)
			if len(parts) != 2 {
				continue
			}
			src, destBase := parts[0], parts[1]
			current := fmt.Sprintf("%s/%s", destBase, timestamp)
			latest := fmt.Sprintf("%s/latest", destBase)

			_ = os.MkdirAll(destBase, 0755)

			args := []string{"-avh", "--delete"}
			if _, err := os.Stat(latest); err == nil {
				// 前回バックアップをベースに、差分のみを物理コピーすることで効率化する。
				args = append(args, "--link-dest", latest)
			}
			args = append(args, src+"/", current)

			if out, err := exec.CommandContext(ctx, "rsync", args...).CombinedOutput(); err != nil {
				logger.Logf("Internal", "Container", "%s: rsync失敗: %v, output: %s", id, err, string(out))
				hasError = true
				continue
			}

			// バックアップ完了後、最新版へのシンボリックリンクを貼り替え、管理を容易にする。
			_ = os.Remove(latest)
			_ = os.Symlink(timestamp, latest)
		}
	}

	if hasError {
		return fmt.Errorf("backup failed partially")
	}
	logger.Logf("Internal", "Container", "バックアップが完了しました: %s", id)
	return nil
}

// MARK: Restore()
// 保存されている最新のバックアップから、データをロールバックする。
func (m *Manager) Restore(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		return fmt.Errorf("server %s not found in config", id)
	}

	// 復旧作業中のデータ競合を防ぐため、一旦コンテナを確実に停止させる必要がある。
	// 起動中のコンテナに対するRestoreは危険なため、エラーとして拒否する。
	if inspect, err := docker.Client.ContainerInspect(ctx, id); err == nil && inspect.State.Running {
		return fmt.Errorf("container is running. please stop it before restore")
	}

	var hasError bool
	for _, cmd := range server.Commands.Backup {
		if cmd.Type != "backup" {
			continue
		}

		parts := strings.SplitN(cmd.Arg, ":", 2)
		if len(parts) != 2 {
			continue
		}
		src, destBase := parts[0], parts[1]
		latest := fmt.Sprintf("%s/latest", destBase)

		if _, err := os.Stat(latest); err != nil {
			// 復元元が存在しない場合は、警告を出しつつ次の項目へ。
			logger.Logf("Internal", "Container", "%s: 復元対象のバックアップが見つかりません: %s", id, latest)
			continue
		}

		// バックアップ時点の状態に完全に一致させるため、rsync の --delete オプション付きで復元する。
		if out, err := exec.CommandContext(ctx, "rsync", "-avh", "--delete", latest+"/", src).CombinedOutput(); err != nil {
			logger.Logf("Internal", "Container", "%s: 復元失敗: %v, output: %s", id, err, string(out))
			hasError = true
		}
	}

	if hasError {
		return fmt.Errorf("restore failed partially")
	}
	logger.Logf("Internal", "Container", "最新のバックアップからの復元が完了しました: %s", id)
	return nil
}
