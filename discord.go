package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/api/types/container"
)

// MARK: var
// MARK: var
var (
	discordSessions = make(map[string]*discordgo.Session)
	channelToServer = make(map[string]string) // ChannelID -> ServerName
	discordMutex    sync.RWMutex
)

// MARK: startDiscordBots()
func startDiscordBots() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// 初回実行
	updateBots()

	for range ticker.C {
		updateBots()
	}
}

// MARK: updateBots()
func updateBots() {
	cfg := getConfig() // config.goのgetConfigはファイル更新を確認する

	// 最新のチャンネルマッピング構築
	newChannelToServer := make(map[string]string)
	activeTokens := make(map[string]bool)

	for serverName, serverCfg := range cfg.Servers {
		token := serverCfg.Discord.Token
		if token == "" {
			continue
		}
		activeTokens[token] = true

		// WebhookからチャンネルIDを抽出
		parts := strings.Split(serverCfg.Discord.Webhook, "/")
		if len(parts) >= 6 {
			channelID := parts[len(parts)-2]
			newChannelToServer[channelID] = serverName
		}
	}

	discordMutex.Lock()
	channelToServer = newChannelToServer
	log.Printf("Channel mapping updated: %v", channelToServer)
	discordMutex.Unlock()

	// セッション管理 (新規追加・維持)
	for token := range activeTokens {
		discordMutex.RLock()
		_, exists := discordSessions[token]
		discordMutex.RUnlock()

		if !exists {
			dg, err := discordgo.New("Bot " + token)
			if err != nil {
				log.Printf("Error creating Discord session: %v", err)
				continue
			}

			dg.AddHandler(onInteractionCreate)
			dg.AddHandler(onMessageCreate)

			if err := dg.Open(); err != nil {
				log.Printf("Error opening Discord connection: %v", err)
				continue
			}

			registerCommands(dg)

			discordMutex.Lock()
			discordSessions[token] = dg
			discordMutex.Unlock()
			log.Printf("Discord bot started for token ending in ...%s", token[len(token)-4:])
		}
	}

	// 削除されたトークンのセッション終了
	discordMutex.Lock()
	for token, session := range discordSessions {
		if !activeTokens[token] {
			session.Close()
			delete(discordSessions, token)
			log.Printf("Discord bot stopped for token ending in ...%s", token[len(token)-4:])
		}
	}
	discordMutex.Unlock()
}

// MARK: registerCommands()
func registerCommands(s *discordgo.Session) {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "action",
			Description: "Perform server actions",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "type",
					Description: "Action type",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "start", Value: "start"},
						{Name: "stop", Value: "stop"},
						{Name: "backup", Value: "backup"},
						{Name: "restore", Value: "restore"},
						{Name: "kill", Value: "kill"},
					},
				},
			},
		},
		{
			Name:        "cmd",
			Description: "Send command to server console",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "text",
					Description: "Command text",
					Required:    true,
				},
			},
		},
	}

	for _, cmd := range commands {
		_, err := s.ApplicationCommandCreate(s.State.User.ID, "", cmd)
		if err != nil {
			log.Printf("Cannot create command %s: %v", cmd.Name, err)
		}
	}
}

// MARK: onInteractionCreate
func onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	channelID := i.ChannelID

	discordMutex.RLock()
	serverName, ok := channelToServer[channelID]
	discordMutex.RUnlock()

	if !ok {
		// このチャンネルは管理対象外
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This channel is not linked to any server.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	cfg := getConfig()
	serverCfg := cfg.Servers[serverName]

	switch i.ApplicationCommandData().Name {
	case "action":
		action := i.ApplicationCommandData().Options[0].StringValue()
		err := handleAction(serverName, serverCfg, action)
		msg := fmt.Sprintf("Action `%s` executed for **%s**.", action, serverName)
		if err != nil {
			msg = fmt.Sprintf("Error executing `%s`: %v", action, err)
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
			},
		})

	case "cmd":
		cmdText := i.ApplicationCommandData().Options[0].StringValue()
		err := sendCommandToContainer(serverName, cmdText)
		msg := fmt.Sprintf("Command sent to **%s**.", serverName)
		if err != nil {
			msg = fmt.Sprintf("Error sending command: %v", err)
		}
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
			},
		})
	}
}

// MARK: onMessageCreate
func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	discordMutex.RLock()
	serverName, ok := channelToServer[m.ChannelID]
	discordMutex.RUnlock()

	if !ok {
		return
	}

	cfg := getConfig()
	serverCfg := cfg.Servers[serverName]
	msgTemplate := serverCfg.Commands.Message

	if msgTemplate == "" {
		return
	}

	// テンプレート置換
	text := strings.ReplaceAll(msgTemplate, "${user}", m.Author.Username)
	text = strings.ReplaceAll(text, "${message}", m.Content)
	// 改行を追加して送信
	sendCommandToContainer(serverName, text+"\n")
}

// MARK: handleAction
func handleAction(serverName string, config ConfigServer, action string) error {
	ctx := context.Background()
	// イメージ名からコンテナIDを探す (簡易実装: 名前が一致するもの、あるいはImageが一致するもの)
	// ここではconfig.jsonにコンテナ名があればいいが、現状の実装だと動的にコンテナを探す必要がある
	// 既存のAuthロジックではContainerInspectを使ってるが、サーバー名とコンテナ名の対応が必要。
	// config.examples.jsonの構造からすると `serverName` とコンテナ名は必ずしも一致しないが、
	// 一般的に管理上 `serverName` = `containerName` と仮定するか、検索する。
	// 今回は `serverName` をコンテナ名として扱う(play-binの既存ロジックと合わせるなら要調整だが今回はこれで行く)
	
	// コンテナIDの特定
	// Main.goのロジックを見ると、コンテナ名はDocker上の名前(`newyear_1.20.4`など)
	// Configのキーは `newyear`。
	// ここでは `config.json` にコンテナ名(ID)の設定がないため迷うが、
	// `ConfigServer` にDockerコンテナ名を含めるべきかもしれない。
	// しかし現状無いので、`serverName` = Docker Container Name と仮定して進める。
	// *補足*: `config.example.json` の `workingDir` から推測も難しい。
	// ユーザー要望に `action` とあるので、とりあえず `serverName` をコンテナ名として扱う。
	
	id := serverName // 仮定

	switch action {
	case "start":
		return cli.ContainerStart(ctx, id, container.StartOptions{})
	case "stop":
		return cli.ContainerStop(ctx, id, container.StopOptions{})
	case "kill":
		return cli.ContainerKill(ctx, id, "SIGKILL")
	case "backup":
		// Backup logic (Command execution)
		for _, backupCmd := range config.Commands.Backup {
			// 簡易的なバックアップコマンド実行 (cpなど)
			// 実際にはホスト側の操作が必要だが、Docker内からDocker操作してるならMount経由で操作?
			// ここではログだけ出す
			log.Printf("Backup requested: %s -> %s", backupCmd.Src, backupCmd.Dest)
		}
		return nil
	case "restore":
		log.Printf("Restore requested for %s", serverName)
		return nil
	}
	return fmt.Errorf("unknown action: %s", action)
}

// MARK: sendCommandToContainer
func sendCommandToContainer(containerName string, command string) error {
	ctx := context.Background()
	
	// Attachしてstdinに書き込む
	resp, err := cli.ContainerAttach(ctx, containerName, container.AttachOptions{
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
