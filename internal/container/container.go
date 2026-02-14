package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	ctypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
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
	ActionRemove  Action = "remove"
)

// Manager handles high-level container operations
type Manager struct {
	Config *config.LoadedConfig
}

// MARK: ExecuteAction()
// 指定されたアクション（起動、停止など）をコンテナに対して実行する。
func (m *Manager) ExecuteAction(ctx context.Context, serverName string, action Action) error {
	// アクションの種類に応じて、低レベルな個別メソッドに処理を委譲する。
	switch action {
	case ActionStart:
		return m.Start(ctx, serverName)
	case ActionStop:
		return m.Stop(ctx, serverName)
	case ActionKill:
		return m.Kill(ctx, serverName)
	case ActionBackup:
		return m.Backup(ctx, serverName)
	case ActionRestore:
		// restore は世代指定パラメータが必須のため、汎用アクション経由では実行不可。
		return fmt.Errorf("restore requires a generation parameter. use dedicated restore handler")
	case ActionRemove:
		return m.Remove(ctx, serverName)
	default:
		return fmt.Errorf("unknown action: %w", errors.New(string(action)))
	}
}

// MARK: Start()
// コンフィグ情報を元にコンテナを起動する。
// 既に同名のコンテナが存在する場合は、手動での削除を促しエラーを返す。
func (m *Manager) Start(ctx context.Context, serverName string) error {
	cfg := m.Config.Get()
	serverCfg, ok := cfg.Servers[serverName]
	if !ok || serverCfg.Compose == nil || serverCfg.Compose.Image == "" {
		// 設定が存在しない、またはイメージが未定義の場合は、起動対象外として何もしない。
		return nil
	}

	// 既にコンテナが存在するか確認する。
	// 安全のため、ユーザーが明示的に /remove を実行するまで、自動での破壊（再作成）は行わない。
	if inspect, err := docker.Client.ContainerInspect(ctx, serverName); err == nil {
		if inspect.State.Running {
			return fmt.Errorf("container %s is already running. please stop and remove it first", serverName)
		}
		return fmt.Errorf("container %s already exists. please remove it manually to apply new config", serverName)
	} else if !errdefs.IsNotFound(err) {
		// 存在しない(missing)場合のエラー以外は、クリティカルな問題として扱う。
		logger.Logf("Internal", "Container", "コンテナ状態確認失敗(%s): %v", serverName, err)
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	// コンテナのランタイム設定。TTYを有効にすることで、Web経由のターミナル操作を可能にする。
	containerConfig := &ctypes.Config{
		Image:     serverCfg.Compose.Image,
		Tty:       true,
		OpenStdin: true,
	}

	// カスタムの起動コマンドが指定されている場合のみ、エントリポイントや引数を上書きする。
	if serverCfg.Compose.Command != nil {
		if e := serverCfg.Compose.Command.Entrypoint; e != "" {
			containerConfig.Entrypoint = strings.Fields(e)
		}
		if a := serverCfg.Compose.Command.Arguments; a != "" {
			containerConfig.Cmd = strings.Fields(a)
		}
	}

	hostConfig := &ctypes.HostConfig{}

	// 設定された全ディレクトリをホストからコンテナのボリュームとしてマッピングする。
	for hostPath, containerPath := range serverCfg.Compose.Mount {
		hostConfig.Binds = append(hostConfig.Binds, hostPath+":"+containerPath)
	}

	// 異常終了時の自動再起動ポリシーを設定する。
	// デフォルト（未指定）は "no" とし、明示的な指定がある場合のみ適用する。
	restartPolicy := serverCfg.Compose.Restart
	if restartPolicy == "" {
		restartPolicy = "no"
	}
	hostConfig.RestartPolicy = ctypes.RestartPolicy{
		Name: ctypes.RestartPolicyMode(restartPolicy),
	}

	// ネットワーク接続モードの決定。明示的な指定がない場合は、隔離性の高い bridge モードを採用する。
	hostConfig.NetworkMode = ctypes.NetworkMode(serverCfg.Compose.Network.Mode)
	if hostConfig.NetworkMode == "" {
		hostConfig.NetworkMode = "bridge"
	}

	// ブリッジモード使用時のみ、外部公開用のポートフォワーディングを動的に構築する。
	if hostConfig.NetworkMode == "bridge" && len(serverCfg.Compose.Network.Mapping) > 0 {
		portBindings := nat.PortMap{}
		exposedPorts := nat.PortSet{}
		for hostPort, containerPort := range serverCfg.Compose.Network.Mapping {
			port := nat.Port(containerPort + "/tcp")
			exposedPorts[port] = struct{}{}
			portBindings[port] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: hostPort}}
		}
		containerConfig.ExposedPorts = exposedPorts
		hostConfig.PortBindings = portBindings
	}

	// コンテナの実体を Docker エンジン上に生成する。
	if _, err := docker.Client.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, serverName); err != nil {
		logger.Logf("Internal", "Container", "コンテナ作成失敗(%s): %v", serverName, err)
		return fmt.Errorf("failed to create container: %w", err)
	}

	// 生成したコンテナプロセスの実行を開始する。
	if err := docker.Client.ContainerStart(ctx, serverName, ctypes.StartOptions{}); err != nil {
		logger.Logf("Internal", "Container", "コンテナ起動失敗(%s): %v", serverName, err)
		return fmt.Errorf("failed to start container: %w", err)
	}
	logger.Logf("Internal", "Container", "コンテナの起動に成功しました: %s", serverName)
	return nil
}

// MARK: Stop()
// カスタム停止シーケンス（ゲーム内コマンド送信等）を順守しつつ、コンテナを停止する。
func (m *Manager) Stop(ctx context.Context, serverName string) error {
	cfg := m.Config.Get()
	serverCfg, ok := cfg.Servers[serverName]
	if !ok {
		// 管理対象外のコンテナは、標準的な停止命令（SIGTERM 等）のみを発行する。
		return docker.Client.ContainerStop(ctx, serverName, ctypes.StopOptions{})
	}

	// データを安全に保存して終了させるため、Docker 停止前に定義済みのクリーンアップ手順を実行する。
	for _, cmd := range serverCfg.Commands.Stop {
		switch cmd.Type {
		case "attach":
			// コンテナの stdin に直接コマンドを流し込み、アプリケーションレベルの終了処理を促す。
			if err := docker.SendCommand(serverName, cmd.Arg); err != nil {
				logger.Logf("Internal", "Container", "%s: attachコマンド送信失敗: %v", serverName, err)
			}
		case "exec":
			// 外部から補助プロセスを実行してクリーンアップを行う。
			if err := docker.SendExec(serverName, []string{"/bin/sh", "-c", cmd.Arg}); err != nil {
				logger.Logf("Internal", "Container", "%s: exec実行失敗: %v", serverName, err)
			}
		case "log":
			// 運用の透明性を確保するため、重要なフェーズをシステムログに刻む。
			logger.Log("Internal", "Container", fmt.Sprintf("[%s] %s", serverName, cmd.Arg))
		case "sleep":
			// アプリケーションが完全にシャットダウンするまでの猶予期間を確保する。
			if dur, err := time.ParseDuration(cmd.Arg); err == nil {
				time.Sleep(dur)
			}
		}
	}

	// 全ての手順が完了、またはタイムアウト後に、Docker レベルでコンテナを最終停止させる。
	if err := docker.Client.ContainerStop(ctx, serverName, ctypes.StopOptions{}); err != nil {
		logger.Logf("Internal", "Container", "コンテナ停止失敗(%s): %v", serverName, err)
		return fmt.Errorf("failed to stop container: %w", err)
	}
	logger.Logf("Internal", "Container", "コンテナの停止に成功しました: %s", serverName)
	return nil
}

// MARK: Kill()
// 応答不能になったコンテナを、SIGKILL 等を用いて強制的に停止する。
func (m *Manager) Kill(ctx context.Context, serverName string) error {
	timeout := 30
	// 可能な限りリソースを壊さないよう、まずは短いタイムアウト付きで標準的な停止を試みる。
	if err := docker.Client.ContainerStop(ctx, serverName, ctypes.StopOptions{Timeout: &timeout}); err == nil {
		logger.Logf("Internal", "Container", "コンテナが正常に停止しました(Kill経由): %s", serverName)
		return nil
	}
	// 標準停止が失敗した場合、OS レベルでプロセスを強制終了させる。
	err := docker.Client.ContainerKill(ctx, serverName, "SIGKILL")
	if err == nil {
		logger.Logf("Internal", "Container", "コンテナを強制終了しました: %s", serverName)
	} else {
		err = fmt.Errorf("failed to kill container: %w", err)
	}
	return err
}

// MARK: Backup()
// 指定されたパスのデータを、rsync を用いてインクリメンタルにバックアップする。
func (m *Manager) Backup(ctx context.Context, serverName string) error {
	cfg := m.Config.Get()
	serverCfg, ok := cfg.Servers[serverName]
	if !ok {
		return fmt.Errorf("server %s not found in config or not managed by docker", serverName)
	}

	// マシンのタイムゾーンに合わせた世代名を生成する。
	timestamp := time.Now().Local().Format("20060102_150405")
	var hasError bool

	// 整合性のあるバックアップを取得するため、事前に「保存」コマンド等を送信する必要があるかを確認する。
	isRunning := false
	if inspect, err := docker.Client.ContainerInspect(ctx, serverName); err == nil && inspect.State.Running {
		isRunning = true
	}

	for _, cmd := range serverCfg.Commands.Backup {
		switch cmd.Type {
		case "attach":
			// コンテナが起動していない場合は、stdinへのコマンド送信は失敗するためスキップする。
			if !isRunning {
				logger.Logf("Internal", "Container", "%s: コンテナ停止中のためバックアップ準備コマンド(attach)をスキップします", serverName)
				continue
			}
			// ゲームサーバー等の「save-all」コマンドを想定し、ディスクへの同期を促す。
			if err := docker.SendCommand(serverName, cmd.Arg+"\n"); err != nil {
				logger.Logf("Internal", "Container", "%s: バックアップ準備コマンド送信失敗: %v", serverName, err)
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
				logger.Logf("Internal", "Container", "%s: rsync失敗: %v, output: %s", serverName, err, string(out))
				hasError = true
				continue
			}

			// バックアップ完了後、最新版へのシンボリックリンクを貼り替え、管理を容易にする。
			_ = os.Remove(latest)
			_ = os.Symlink(timestamp, latest)
		}
	}

	if hasError {
		return fmt.Errorf("backup failed partially: %w", errors.New("one or more backup steps failed"))
	}
	logger.Logf("Internal", "Container", "バックアップが完了しました: %s", serverName)
	return nil
}

// MARK: ListBackupGenerations()
// バックアップディレクトリ内の世代（タイムスタンプ）一覧を新しい順で返す。
func (m *Manager) ListBackupGenerations(serverName string) ([]string, error) {
	cfg := m.Config.Get()
	serverCfg, ok := cfg.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("server %s not found in config", serverName)
	}

	// 複数のバックアップ定義がある場合は、全ての destBase を横断して世代を集約する。
	seen := make(map[string]bool)
	var generations []string

	for _, cmd := range serverCfg.Commands.Backup {
		if cmd.Type != "backup" {
			continue
		}

		parts := strings.SplitN(cmd.Arg, ":", 2)
		if len(parts) != 2 {
			continue
		}
		destBase := parts[1]

		entries, err := os.ReadDir(destBase)
		if err != nil {
			// ディレクトリ自体がまだ存在しない場合は空として扱う。
			continue
		}

		for _, entry := range entries {
			// latest シンボリックリンクは世代一覧から除外する。
			if entry.Name() == "latest" {
				continue
			}
			if !entry.IsDir() {
				continue
			}
			if !seen[entry.Name()] {
				seen[entry.Name()] = true
				generations = append(generations, entry.Name())
			}
		}
	}

	// タイムスタンプ形式のため、降順ソートで最新が先頭に来る。
	sort.Sort(sort.Reverse(sort.StringSlice(generations)))
	return generations, nil
}

// MARK: Restore()
// 指定された世代のバックアップからデータをロールバックする。
// generation は必須であり、空文字の場合はエラーを返す。
func (m *Manager) Restore(ctx context.Context, serverName string, generation string) error {
	if generation == "" {
		return fmt.Errorf("generation is required for restore")
	}

	cfg := m.Config.Get()
	serverCfg, ok := cfg.Servers[serverName]
	if !ok {
		return fmt.Errorf("server %s not found in config", serverName)
	}

	// 復旧作業中のデータ競合を防ぐため、一旦コンテナを確実に停止させる必要がある。
	// 起動中のコンテナに対するRestoreは危険なため、エラーとして拒否する。
	if inspect, err := docker.Client.ContainerInspect(ctx, serverName); err == nil && inspect.State.Running {
		return fmt.Errorf("container is running. please stop it before restore")
	}

	var hasError bool
	for _, cmd := range serverCfg.Commands.Backup {
		if cmd.Type != "backup" {
			continue
		}

		parts := strings.SplitN(cmd.Arg, ":", 2)
		if len(parts) != 2 {
			continue
		}
		src, destBase := parts[0], parts[1]

		// 必須パラメータとして受け取った世代名のディレクトリから復元する。
		restoreSrc := filepath.Join(destBase, generation)

		if _, err := os.Stat(restoreSrc); err != nil {
			// 復元元が存在しない場合は、警告を出しつつ次の項目へ。
			logger.Logf("Internal", "Container", "%s: 復元対象のバックアップが見つかりません: %s", serverName, restoreSrc)
			continue
		}

		// バックアップ時点の状態に完全に一致させるため、rsync の --delete オプション付きで復元する。
		if out, err := exec.CommandContext(ctx, "rsync", "-avh", "--delete", restoreSrc+"/", src).CombinedOutput(); err != nil {
			logger.Logf("Internal", "Container", "%s: 復元失敗: %v, output: %s", serverName, err, string(out))
			hasError = true
		}
	}

	if hasError {
		return fmt.Errorf("restore failed partially")
	}

	logger.Logf("Internal", "Container", "世代 %s からの復元が完了しました: %s", generation, serverName)
	return nil
}

// MARK: Remove()
// 停止状態のコンテナを、Docker エンジンから物理的に削除する。
func (m *Manager) Remove(ctx context.Context, serverName string) error {
	// 誤って稼働中のサービスを破壊しないよう、事前に実行状態を厳密にチェックする。
	if inspect, err := docker.Client.ContainerInspect(ctx, serverName); err == nil {
		if inspect.State.Running {
			// 稼働中の場合は削除を拒否し、ユーザーに停止を促す。
			return fmt.Errorf("container is running. please stop/kill it before remove")
		}
	} else if errdefs.IsNotFound(err) {
		// 既に存在しない場合は、目的が達成されているため成功として扱う。
		return nil
	} else {
		logger.Logf("Internal", "Container", "コンテナ状態確認失敗(%s): %v", serverName, err)
		return fmt.Errorf("failed to check container state: %w", err)
	}

	// Docker SDK を呼び出し、コンテナを破棄する。
	if err := docker.Client.ContainerRemove(ctx, serverName, ctypes.RemoveOptions{}); err != nil {
		logger.Logf("Internal", "Container", "コンテナ削除失敗(%s): %v", serverName, err)
		return fmt.Errorf("failed to remove container: %w", err)
	}

	logger.Logf("Internal", "Container", "コンテナを削除しました: %s", serverName)
	return nil
}
