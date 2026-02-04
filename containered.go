package main

import (
	"context"

	"github.com/docker/docker/api/types/container"
)

// MARK: startContainer()
func startContainer(id string) {
	config := getConfig()
	server, ok := config.Servers[id]
	if !ok || server.Image == "" {
		return
	}

	cli.ContainerCreate(context.Background(), &container.Config{
		Image: server.Image,
	}, &container.HostConfig{}, nil, nil, id)
	cli.ContainerStart(context.Background(), id, container.StartOptions{})
}
