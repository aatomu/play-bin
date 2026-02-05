package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/api/types/container"
)

type embedType int

const (
	EmbedTypeError embedType = iota
	EmbedTypeWarn
	EmbedTypeSuccess
	EmbedTypeInfo
)

const (
	colorError   = 0xFF2929
	colorWarn    = 0xFFC107
	colorSuccess = 0x6EC207
	colorInfo    = 0x00b0ff
)

// MARK: startDiscordBots()
func startDiscordBots() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// 初回実行
	updateBots()

	for range ticker.C {
		updateBots()
	}
}

// MARK: updateBots()
func updateBots() {
	cfg := config.Get()

	// 更新するか確認
	if !channelUpdatedAt.Before(config.lastLoaded) {
		return
	}
	channelUpdatedAt = config.lastLoaded

	// 最新のチャンネルマッピング構築
	newChannelToServer := make(map[string]string)
	activeTokens := make(map[string]bool)

	for serverName, serverCfg := range cfg.Servers {
		token := serverCfg.Discord.Token
		if token == "" {
			continue
		}
		activeTokens[token] = true

		// チャンネルIDを直接使用
		channelID := serverCfg.Discord.Channel
		if channelID != "" {
			newChannelToServer[channelID] = serverName
		}
	}

	discordMu.Lock()
	channelToServer = newChannelToServer
	log.Printf("Channel mapping updated: %v", channelToServer)
	discordMu.Unlock()

	// セッション管理 (新規追加・維持)
	for token := range activeTokens {
		discordMu.RLock()
		_, exists := discordSessions[token]
		discordMu.RUnlock()

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

			discordMu.Lock()
			discordSessions[token] = dg
			discordMu.Unlock()
			log.Printf("Discord bot started for token ending in ...%s", token[len(token)-4:])
		}
	}

	// 削除されたトークンのセッション終了
	discordMu.Lock()
	for token, session := range discordSessions {
		if !activeTokens[token] {
			session.Close()
			delete(discordSessions, token)
			log.Printf("Discord bot stopped for token ending in ...%s", token[len(token)-4:])
		}
	}
	discordMu.Unlock()
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

	discordMu.RLock()
	serverName, ok := channelToServer[channelID]
	discordMu.RUnlock()

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

	cfg := config.Get()

	// 操作権限チェック
	discordUserID := ""
	if i.Member != nil && i.Member.User != nil {
		discordUserID = i.Member.User.ID
	} else if i.User != nil {
		discordUserID = i.User.ID
	}

	hasPermission := false
	for _, user := range cfg.Users {
		if user.Discord == discordUserID {
			for _, pattern := range user.Controllable {
				if pattern == "*" || pattern == serverName {
					hasPermission = true
					break
				}
			}
			break
		}
	}

	if !hasPermission {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You don't have permission to control this server.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	switch i.ApplicationCommandData().Name {
	case "action":
		action := i.ApplicationCommandData().Options[0].StringValue()
		err := containerAction(serverName, ContainerAction(action))
		if err != nil {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: interactionError(action, err),
			})
			return
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: interactionSuccess(action, "Wait to be starting..."),
		})
	case "cmd":
		cmdText := i.ApplicationCommandData().Options[0].StringValue()
		err := sendCommandToContainer(serverName, cmdText)
		if err != nil {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: interactionError("command", err),
			})
			return
		}
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: interactionSuccess("command", ""),
		})
	}
}

// MARK: onMessageCreate
func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot || m.Author.ID == s.State.User.ID {
		return
	}

	discordMu.RLock()
	serverName, ok := channelToServer[m.ChannelID]
	discordMu.RUnlock()

	if !ok {
		return
	}

	cfg := config.Get()
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

// MARK: sendCommandToContainer
func sendCommandToContainer(containerName string, command string) error {
	ctx := context.Background()

	// Attachしてstdinに書き込む
	resp, err := dockerCli.ContainerAttach(ctx, containerName, container.AttachOptions{
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

func interactionError(action string, err error) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{
		Embeds: []*discordgo.MessageEmbed{
			&discordgo.MessageEmbed{
				Color:       colorError,
				Title:       fmt.Sprintf("Execution Error: %s", action),
				Description: err.Error(),
			},
		},
		Flags: discordgo.MessageFlagsEphemeral,
	}
}

func interactionSuccess(action string, description string) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{
		Embeds: []*discordgo.MessageEmbed{
			&discordgo.MessageEmbed{
				Color:       colorSuccess,
				Title:       fmt.Sprintf("Execution Success: %s", action),
				Description: description,
			},
		},
	}
}

// func createEmbed(title string, description string, iconUrl string, color embedType) *discordgo.MessageEmbed {
// 	var embedColor int
// 	switch color {
// 	case EmbedTypeError:
// 		embedColor = colorError
// 	case EmbedTypeWarn:
// 		embedColor = colorWarn
// 	case EmbedTypeSuccess:
// 		embedColor = colorSuccess
// 	case EmbedTypeInfo:
// 		embedColor = colorInfo
// 	}

// 	if iconUrl == "" {
// 		return &discordgo.MessageEmbed{
// 			Title:       title,
// 			Description: description,
// 			Color:       embedColor,
// 		}
// 	} else {
// 		return &discordgo.MessageEmbed{
// 			Title:       title,
// 			Description: description,
// 			Color:       embedColor,
// 			Author: &discordgo.MessageEmbedAuthor{
// 				Name:    "PlayBin",
// 				IconURL: iconUrl,
// 			},
// 		}
// 	}
// }
