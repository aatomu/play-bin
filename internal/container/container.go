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
	// アクションの種類に応じて呼び出し先を振り分ける
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
		return fmt.Errorf("未知のアクションです: %s", action)
	}
}

// MARK: Start()
// コンフィグ情報を元にコンテナを作成・起動する。
func (m *Manager) Start(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok || server.Image == "" {
		// 設定が存在しない、またはイメージ指定がない場合は起動できないため早期リターン
		return nil
	}

	// コンテナの基本設定（TtyやOpenStdinを有効にすることでターミナル操作を可能にする）
	containerConfig := &ctypes.Config{
		Image:     server.Image,
		Tty:       true,
		OpenStdin: true,
	}

	// 特定の起動コマンドが必要な場合（例：Javaのメモリオプション指定など）のみ設定を適用する
	if e := server.Commands.Start.Entrypoint; e != "" {
		containerConfig.Entrypoint = strings.Fields(e)
	}
	if a := server.Commands.Start.Arguments; a != "" {
		containerConfig.Cmd = strings.Fields(a)
	}

	// ホスト側のリソース指定（マウント、ネットワーク）
	hostConfig := &ctypes.HostConfig{}

	// 設定されたディレクトリをホストからコンテナにマウントする
	for hostPath, containerPath := range server.Mount {
		hostConfig.Binds = append(hostConfig.Binds, hostPath+":"+containerPath)
	}

	// ネットワーク接続モードの決定。未指定時はデフォルトの 'bridge' を使用。
	hostConfig.NetworkMode = ctypes.NetworkMode(server.Network.Mode)
	if hostConfig.NetworkMode == "" {
		hostConfig.NetworkMode = "bridge"
	}

	// ブリッジモードの場合のみ、外部からアクセス可能にするためのポートマッピングを設定する
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

	// 冪等性を保つため、古いコンテナが残っている場合は一旦削除する処理。
	// これによりコンフィグ変更を反映した新しいコンテナを作成できる。
	if inspect, err := docker.Client.ContainerInspect(ctx, id); err == nil {
		if !inspect.State.Running {
			// ボリュームなどは残さず、コンテナ自体のインスタンスのみをクリーンアップする
			_ = docker.Client.ContainerRemove(ctx, id, ctypes.RemoveOptions{})
		}
	} else if !client.IsErrNotFound(err) {
		logger.Error("Container", err)
		return err
	}

	// コンテナの生成。ここで設定したコンフィグが実際のDockerインスタンスに適用される。
	if _, err := docker.Client.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, id); err != nil {
		logger.Logf("Internal", "Container", "コンテナ作成失敗(%s): %v", id, err)
		return err
	}

	// 生成したコンテナを起動。
	return docker.Client.ContainerStart(ctx, id, ctypes.StartOptions{})
}

// MARK: Stop()
// カスタム停止ロジック（コマンド送信等）を実行した後にコンテナを停止する。
func (m *Manager) Stop(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		// 設定がない場合は通常のDocker停止を試みる（外部作成コンテナへの対応）
		return docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{})
	}

	// アプリケーションを安全に終了させるため、DockerのSIGTERM前にカスタムコマンド（例：/stopコマンドの送信）を実行する。
	for _, cmd := range server.Commands.Stop {
		switch cmd.Type {
		case "attach":
			if err := docker.SendCommand(id, cmd.Arg); err != nil {
				logger.Logf("Internal", "Container", "%s: attachコマンド送信失敗: %v", id, err)
			}
		case "exec":
			if err := docker.SendExec(id, []string{"/bin/sh", "-c", cmd.Arg}); err != nil {
				logger.Logf("Internal", "Container", "%s: exec実行失敗: %v", id, err)
			}
		case "log":
			logger.Log("Internal", "Container", fmt.Sprintf("%s: %s", id, cmd.Arg))
		case "sleep":
			// アプリのシャットダウンが完了するまでの待機時間を設ける。
			if dur, err := time.ParseDuration(cmd.Arg); err == nil {
				time.Sleep(dur)
			}
		}
	}

	// 最後にDockerエンジンに対して明示的な停止命令を出す。
	return docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{})
}

// MARK: Kill()
// コンテナを強制停止する。
func (m *Manager) Kill(ctx context.Context, id string) error {
	timeout := 30
	// まずは穏便に停止を試み、タイムアウトした場合は強制終了（SIGKILL）を試みる。
	if err := docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{Timeout: &timeout}); err == nil {
		return nil
	}
	return docker.Client.ContainerKill(ctx, id, "SIGKILL")
}

// MARK: Backup()
// 設定されたパスのバックアップを取得する。
func (m *Manager) Backup(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		return fmt.Errorf("サーバー %s が見つかりません", id)
	}

	timestamp := time.Now().Format("20060102_150405")
	var hasError bool

	// データ整合性を保つため、バックアップ前には必要に応じてコマンド送信や待機を繰り返す。
	for _, cmd := range server.Commands.Backup {
		switch cmd.Type {
		case "attach":
			if err := docker.SendCommand(id, cmd.Arg+"\n"); err != nil {
				logger.Logf("Internal", "Container", "%s: バックアップ準備コマンド送信失敗: %v", id, err)
			}
		case "sleep":
			if dur, err := time.ParseDuration(cmd.Arg); err == nil {
				time.Sleep(dur)
			}
		case "backup":
			// rsyncのハードリンク機能を利用して、変更がないファイルはリンクに留めることでディスク容量を節約する。
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
				// 前回バックアップがあれば、それとの差分のみを新規作成する
				args = append(args, "--link-dest", latest)
			}
			args = append(args, src+"/", current)

			if out, err := exec.CommandContext(ctx, "rsync", args...).CombinedOutput(); err != nil {
				logger.Logf("Internal", "Container", "%s: rsync失敗: %v, output: %s", id, err, string(out))
				hasError = true
				continue
			}

			// 最新版へのリンクを更新して次回のバックアップに備える。
			_ = os.Remove(latest)
			_ = os.Symlink(timestamp, latest)
		}
	}

	if hasError {
		return fmt.Errorf("バックアップ処理中にエラーが発生しました")
	}
	return nil
}

// MARK: Restore()
// 保存された最新のバックアップからファイルを復元する。
func (m *Manager) Restore(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		return fmt.Errorf("サーバー %s が見つかりません", id)
	}

	// 復元中にデータが破損するのを防ぐため、必ずコンテナを停止した状態で実行する。
	timeout := 30
	_ = docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{Timeout: &timeout})

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
			logger.Logf("Internal", "Container", "%s: 復元対象のバックアップが見つかりません: %s", id, latest)
			continue
		}

		// バックアップから書き戻す際も rsync を使用して差分のみを更新し、完了まで待機する。
		if out, err := exec.CommandContext(ctx, "rsync", "-avh", "--delete", latest+"/", src).CombinedOutput(); err != nil {
			logger.Logf("Internal", "Container", "%s: 復元失敗: %v, output: %s", id, err, string(out))
			hasError = true
		}
	}

	if hasError {
		return fmt.Errorf("復元処理中にエラーが発生しました")
	}
	return nil
}
