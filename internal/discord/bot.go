package discord

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/docker"
)

const (
	colorError   = 0xFF2929
	colorWarn    = 0xFFC107
	colorSuccess = 0x6EC207
	colorInfo    = 0x00b0ff
)

// MARK: SyncBots()
func (m *BotManager) SyncBots() {
	cfg := m.Config.Get()

	m.mu.RLock()
	needsUpdate := m.ChannelUpdatedAt.Before(m.Config.LastLoaded)
	m.mu.RUnlock()

	if !needsUpdate {
		return
	}

	m.mu.Lock()
	m.ChannelUpdatedAt = m.Config.LastLoaded
	m.mu.Unlock()

	newChannelToServer := make(map[string]string)
	activeTokens := make(map[string]bool)

	for serverName, serverCfg := range cfg.Servers {
		token := serverCfg.Discord.Token
		if token == "" {
			continue
		}
		activeTokens[token] = true
		if channelID := serverCfg.Discord.Channel; channelID != "" {
			newChannelToServer[channelID] = serverName
		}
	}

	m.mu.Lock()
	m.ChannelToServer = newChannelToServer
	log.Printf("Channel mapping updated: %v", m.ChannelToServer)
	m.mu.Unlock()

	for token := range activeTokens {
		m.mu.RLock()
		_, exists := m.Sessions[token]
		m.mu.RUnlock()

		if !exists {
			dg, err := discordgo.New("Bot " + token)
			if err != nil {
				log.Printf("Error creating Discord session: %v", err)
				continue
			}

			dg.AddHandler(m.onInteractionCreate)
			dg.AddHandler(m.onMessageCreate)

			if err := dg.Open(); err != nil {
				log.Printf("Error opening Discord connection: %v", err)
				continue
			}

			m.registerCommands(dg)

			m.mu.Lock()
			m.Sessions[token] = dg
			m.mu.Unlock()
			log.Printf("Discord bot started for token ending in ...%s", token[len(token)-4:])
		}
	}

	m.mu.Lock()
	for token, session := range m.Sessions {
		if !activeTokens[token] {
			session.Close()
			delete(m.Sessions, token)
			log.Printf("Discord bot stopped for token ending in ...%s", token[len(token)-4:])
		}
	}
	m.mu.Unlock()
}

// MARK: registerCommands()
func (m *BotManager) registerCommands(dg *discordgo.Session) {
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
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, "", cmd)
		if err != nil {
			log.Printf("Cannot create command %s: %v", cmd.Name, err)
		}
	}
}

// MARK: onInteractionCreate
func (m *BotManager) onInteractionCreate(dg *discordgo.Session, i *discordgo.InteractionCreate) {
	m.mu.RLock()
	serverName, ok := m.ChannelToServer[i.ChannelID]
	m.mu.RUnlock()

	if !ok {
		dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This channel is not linked to any server.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	cfg := m.Config.Get()
	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}

	allowed := false
	for _, user := range cfg.Users {
		if user.Discord == userID {
			for _, pat := range user.Controllable {
				if pat == "*" || pat == serverName {
					allowed = true
					break
				}
			}
			break
		}
	}

	if !allowed {
		dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "You don't have permission to control this server.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// MARK: > Thinking...
	err := dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("Failed to defer interaction: %v", err)
		return
	}

	switch i.ApplicationCommandData().Name {
	case "action":
		act := i.ApplicationCommandData().Options[0].StringValue()
		err := m.ContainerManager.ExecuteAction(context.Background(), serverName, container.Action(act))
		if err != nil {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed(act, err)},
			})
			return
		}
		dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{m.interactionSuccessEmbed(act, "SUCCESS")},
		})
	case "cmd":
		text := i.ApplicationCommandData().Options[0].StringValue()
		err := docker.SendCommand(serverName, text+"\n")
		if err != nil {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed("command", err)},
			})
			return
		}
		dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{m.interactionSuccessEmbed("command", "")},
		})
	}
}

// MARK: onMessageCreate
func (m *BotManager) onMessageCreate(dg *discordgo.Session, msg *discordgo.MessageCreate) {
	if msg.Author.Bot || msg.Author.ID == dg.State.User.ID {
		return
	}

	m.mu.RLock()
	serverName, ok := m.ChannelToServer[msg.ChannelID]
	m.mu.RUnlock()

	if !ok {
		return
	}

	cfg := m.Config.Get()
	serverCfg := cfg.Servers[serverName]
	template := serverCfg.Commands.Message
	if template == "" {
		return
	}

	text := strings.ReplaceAll(template, "${user}", msg.Author.Username)
	text = strings.ReplaceAll(text, "${message}", msg.Content)
	docker.SendCommand(serverName, text+"\n")
}

func (m *BotManager) interactionErrorEmbed(act string, err error) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color:       colorError,
		Title:       fmt.Sprintf("Execution Error: %s", act),
		Description: err.Error(),
	}
}

func (m *BotManager) interactionSuccessEmbed(act string, desc string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color:       colorSuccess,
		Title:       fmt.Sprintf("Execution Success: %s", act),
		Description: desc,
	}
}
