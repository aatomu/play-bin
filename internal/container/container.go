package container

import (
	"context"
	"fmt"
	"log"
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
func (m *Manager) ExecuteAction(ctx context.Context, id string, action Action) error {
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
// configからマウント、ネットワーク設定、起動コマンドを設定してコンテナを作成・起動
func (m *Manager) Start(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok || server.Image == "" {
		return nil
	}

	// MARK: > ContainerConfig
	containerConfig := &ctypes.Config{
		Image:     server.Image,
		Tty:       true,
		OpenStdin: true,
	}

	// 起動コマンド設定
	if server.Commands.Start.Entrypoint != "" {
		containerConfig.Entrypoint = strings.Fields(server.Commands.Start.Entrypoint)
	}
	if server.Commands.Start.Arguments != "" {
		containerConfig.Cmd = strings.Fields(server.Commands.Start.Arguments)
	}

	// MARK: > HostConfig
	hostConfig := &ctypes.HostConfig{}

	// マウント設定
	binds := []string{}
	for hostPath, containerPath := range server.Mount {
		binds = append(binds, hostPath+":"+containerPath)
	}
	hostConfig.Binds = binds

	// ネットワーク設定
	switch server.Network.Mode {
	case "host":
		hostConfig.NetworkMode = "host"
	case "bridge":
		hostConfig.NetworkMode = "bridge"
		// ポートマッピング
		if len(server.Network.Mapping) > 0 {
			portBindings := nat.PortMap{}
			exposedPorts := nat.PortSet{}
			for hostPort, containerPort := range server.Network.Mapping {
				port := nat.Port(containerPort + "/tcp")
				exposedPorts[port] = struct{}{}
				portBindings[port] = []nat.PortBinding{
					{HostIP: "0.0.0.0", HostPort: hostPort},
				}
			}
			containerConfig.ExposedPorts = exposedPorts
			hostConfig.PortBindings = portBindings
		}
	default:
		hostConfig.NetworkMode = "bridge"
	}

	// MARK: > Check Existing Container
	inspect, err := docker.Client.ContainerInspect(ctx, id)
	if err == nil {
		if !inspect.State.Running {
			removeOptions := ctypes.RemoveOptions{
				RemoveVolumes: false,
				RemoveLinks:   false,
				Force:         false,
			}
			if err := docker.Client.ContainerRemove(ctx, id, removeOptions); err != nil {
				return err
			}
		}
	} else if !client.IsErrNotFound(err) {
		return err
	}

	// MARK: > Create and Start
	_, err = docker.Client.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, id)
	if err != nil {
		return err
	}

	return docker.Client.ContainerStart(ctx, id, ctypes.StartOptions{})
}

// MARK: Stop()
func (m *Manager) Stop(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok || server.Image == "" {
		return nil
	}

	for _, cmd := range server.Commands.Stop {
		switch cmd.Type {
		case "attach":
			if err := docker.SendCommand(id, cmd.Arg); err != nil {
				log.Printf("Failed to send command to container %s: %v", id, err)
			}
		case "exec":
			if err := docker.SendExec(id, []string{"/bin/sh", "-c", cmd.Arg}); err != nil {
				log.Printf("Failed to exec command in container %s: %v", id, err)
			}
		case "log":
			log.Printf("[%s] %s", id, cmd.Arg)
		case "sleep":
			dur, err := time.ParseDuration(cmd.Arg)
			if err != nil {
				return err
			}
			time.Sleep(dur)
		}
	}

	return docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{})
}

// MARK: Kill()
func (m *Manager) Kill(ctx context.Context, id string) error {
	timeout := 30
	err := docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{Timeout: &timeout})
	if err == nil {
		return nil
	}

	return docker.Client.ContainerKill(ctx, id, "SIGKILL")
}

// MARK: Backup()
func (m *Manager) Backup(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		return fmt.Errorf("server %s not found", id)
	}

	// 停止して整合性を確保
	timeout := 30
	_ = docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{Timeout: &timeout})

	timestamp := time.Now().Format("20060102_150405")
	success := true

	for _, b := range server.Commands.Backup {
		destDir := b.Dest
		currentDest := fmt.Sprintf("%s/%s", destDir, timestamp)
		latestLink := fmt.Sprintf("%s/latest", destDir)

		if err := os.MkdirAll(destDir, 0755); err != nil {
			log.Printf("Failed to create backup dir %s: %v", destDir, err)
			success = false
			continue
		}

		args := []string{"-av", "--delete"}
		if _, err := os.Stat(latestLink); err == nil {
			args = append(args, "--link-dest", latestLink)
		}
		args = append(args, b.Src+"/", currentDest)

		cmd := exec.CommandContext(ctx, "rsync", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Backup failed for %s: %v\nOutput: %s", b.Src, err, string(output))
			success = false
			continue
		}

		// latestリンクの更新
		_ = os.Remove(latestLink)
		if err := os.Symlink(timestamp, latestLink); err != nil {
			log.Printf("Failed to update latest link for %s: %v", b.Dest, err)
		}
	}

	if !success {
		return fmt.Errorf("backup partially or fully failed")
	}

	return nil
}

// MARK: Restore()
func (m *Manager) Restore(ctx context.Context, id string) error {
	cfg := m.Config.Get()
	server, ok := cfg.Servers[id]
	if !ok {
		return fmt.Errorf("server %s not found", id)
	}

	// 実行中の場合は停止
	timeout := 30
	_ = docker.Client.ContainerStop(ctx, id, ctypes.StopOptions{Timeout: &timeout})

	success := true
	for _, b := range server.Commands.Backup {
		latestLink := fmt.Sprintf("%s/latest", b.Dest)
		if _, err := os.Stat(latestLink); err != nil {
			log.Printf("Latest backup not found for %s", b.Src)
			// success = false // 続行
			continue
		}

		// rsyncで元に戻す
		args := []string{"-av", "--delete", latestLink + "/", b.Src}
		cmd := exec.CommandContext(ctx, "rsync", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Printf("Restore failed for %s: %v\nOutput: %s", b.Src, err, string(output))
			success = false
		}
	}

	if !success {
		return fmt.Errorf("restore partially or fully failed")
	}

	return nil
}
