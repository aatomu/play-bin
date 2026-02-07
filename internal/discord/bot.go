package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/play-bin/internal/container"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

const (
	colorError   = 0xFF2929
	colorWarn    = 0xFFC107
	colorSuccess = 0x6EC207
	colorInfo    = 0x00b0ff
)

// MARK: SyncBots()
// 設定ファイルの内容に合わせて、Discord Botセッションの追加や削除を同期する。
func (m *BotManager) SyncBots() {
	cfg := m.Config.Get()

	m.mu.RLock()
	// 設定ファイルが最後に読み込まれた時刻をチェックし、更新が必要か判断する
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

	// 設定にある全サーバーからDiscord設定を抽出し、有効なトークンをリストアップする
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
	m.mu.Unlock()

	// 新しく追加されたトークンに対して Bot セッションを開始する
	for token := range activeTokens {
		m.mu.RLock()
		_, exists := m.Sessions[token]
		m.mu.RUnlock()

		if !exists {
			dg, err := discordgo.New("Bot " + token)
			if err != nil {
				logger.Logf("External", "Discord", "セッション作成失敗: %v", err)
				continue
			}

			dg.AddHandler(m.onInteractionCreate)
			dg.AddHandler(m.onMessageCreate)

			if err := dg.Open(); err != nil {
				logger.Logf("External", "Discord", "接続オープン失敗: %v", err)
				continue
			}

			m.registerCommands(dg)

			m.mu.Lock()
			m.Sessions[token] = dg
			m.mu.Unlock()
			logger.Logf("Internal", "Discord", "Botを開始しました (token終端: ...%s)", token[len(token)-4:])
		}
	}

	// 設定から削除されたトークンのセッションをクローズしてクリーンアップする
	m.mu.Lock()
	for token, session := range m.Sessions {
		if !activeTokens[token] {
			session.Close()
			delete(m.Sessions, token)
			logger.Logf("Internal", "Discord", "Botを停止しました (token終端: ...%s)", token[len(token)-4:])
		}
	}
	m.mu.Unlock()
}

// MARK: registerCommands()
// スラッシュコマンド（/action, /cmd）をDiscord側に登録する。
func (m *BotManager) registerCommands(dg *discordgo.Session) {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "action",
			Description: "コンテナに対する操作（起動・停止・バックアップ等）を実行します",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "type",
					Description: "実行するアクションを選択",
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
			Description: "サーバーコンソールにコマンドを送信します",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "text",
					Description: "送信するコマンド文字列",
					Required:    true,
				},
			},
		},
	}

	for _, cmd := range commands {
		_, err := dg.ApplicationCommandCreate(dg.State.User.ID, "", cmd)
		if err != nil {
			logger.Logf("External", "Discord", "コマンド登録失敗 (%s): %v", cmd.Name, err)
		}
	}
}

// MARK: onInteractionCreate()
// スラッシュコマンドが実行された際のコールバック処理。権限確認とアクション実行を行う。
func (m *BotManager) onInteractionCreate(dg *discordgo.Session, i *discordgo.InteractionCreate) {
	m.mu.RLock()
	serverName, ok := m.ChannelToServer[i.ChannelID]
	m.mu.RUnlock()

	// チャンネルがどのサーバーにも紐付いていない場合は応答して終了
	if !ok {
		dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "このチャンネルはどのサーバーとも連携されていません。",
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

	// 実行者が対象コンテナの操作権限を持っているか検証
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
		logger.Logf("Client", "Discord", "不正アクセス試行: user=%s, target=%s", userID, serverName)
		dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "あなたにはこのサーバーを操作する権限がありません。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// 処理に時間がかかる可能性があるため、一旦「考え中...」の状態にする
	err := dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		logger.Logf("External", "Discord", "インタラクション応答失敗: %v", err)
		return
	}

	switch i.ApplicationCommandData().Name {
	case "action":
		act := i.ApplicationCommandData().Options[0].StringValue()
		logger.Logf("Client", "Discord", "アクション実行: user=%s, action=%s, target=%s", userID, act, serverName)
		err := m.ContainerManager.ExecuteAction(context.Background(), serverName, container.Action(act))
		if err != nil {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed(act, err)},
			})
			return
		}
		dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{m.interactionSuccessEmbed(act, "実行が完了しました")},
		})
	case "cmd":
		text := i.ApplicationCommandData().Options[0].StringValue()
		logger.Logf("Client", "Discord", "コマンド送信: user=%s, target=%s, text=%s", userID, serverName, text)
		err := docker.SendCommand(serverName, text+"\n")
		if err != nil {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed("command", err)},
			})
			return
		}
		dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{m.interactionSuccessEmbed("command", "コマンドを送信しました")},
		})
	}
}

// MARK: onMessageCreate()
// チャンネルにメッセージが投稿された際、自動返信テンプレートに基づいてコンテナに入力する。
func (m *BotManager) onMessageCreate(dg *discordgo.Session, msg *discordgo.MessageCreate) {
	// 自身のメッセージやBotのメッセージには反応しない（無限ループ防止）
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

	// テンプレート変数を実際のユーザー名と内容で置換し、コンテナの標準入力へ流し込む
	text := strings.ReplaceAll(template, "${user}", msg.Author.Username)
	text = strings.ReplaceAll(text, "${message}", msg.Content)
	docker.SendCommand(serverName, text+"\n")
}

// MARK: interactionErrorEmbed()
// 失敗時の埋め込みメッセージを生成する。
func (m *BotManager) interactionErrorEmbed(act string, err error) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color:       colorError,
		Title:       fmt.Sprintf("実行エラー: %s", act),
		Description: err.Error(),
	}
}

// MARK: interactionSuccessEmbed()
// 成功時の埋め込みメッセージを生成する。
func (m *BotManager) interactionSuccessEmbed(act string, desc string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color:       colorSuccess,
		Title:       fmt.Sprintf("実行成功: %s", act),
		Description: desc,
	}
}
