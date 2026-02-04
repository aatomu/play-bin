package main

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

// MARK: startContainer()
// configからマウント、ネットワーク設定、起動コマンドを設定してコンテナを作成・起動
func startContainer(id string) error {
	config := config.Get()
	server, ok := config.Servers[id]
	if !ok || server.Image == "" {
		return nil
	}

	ctx := context.Background()

	// MARK: > ContainerConfig
	containerConfig := &container.Config{
		Image:     server.Image,
		Tty:       true,
		OpenStdin: true,
	}

	// 起動コマンド設定
	if server.Commands.Start != "" {
		containerConfig.Cmd = strings.Fields(server.Commands.Start)
	}

	// MARK: > HostConfig
	hostConfig := &container.HostConfig{}

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
		// デフォルトはbridge
		hostConfig.NetworkMode = "bridge"
	}

	// MARK: > Create and Start
	_, err := dockerCli.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, id)
	if err != nil {
		return err
	}

	return dockerCli.ContainerStart(ctx, id, container.StartOptions{})
}
