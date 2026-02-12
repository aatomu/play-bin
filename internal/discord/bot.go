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
	// 設定ファイルが最後に読み込まれた時刻をチェックし、Bot構成の更新が必要か判断する。
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

	// 設定にある各サーバーから、有効なDiscordトークンとチャンネルIDの紐付けを抽出する。
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

	// 新たに追加されたトークンに対して、個別の Bot セッションを立ち上げる。
	for token := range activeTokens {
		m.mu.RLock()
		_, exists := m.Sessions[token]
		m.mu.RUnlock()

		if !exists {
			dg, err := discordgo.New("Bot " + token)
			if err != nil {
				// トークン不正等の外部要因（External）として記録。
				logger.Logf("External", "Discord", "セッション作成失敗: %v", err)
				continue
			}

			dg.AddHandler(m.onInteractionCreate)
			dg.AddHandler(m.onMessageCreate)

			if err := dg.Open(); err != nil {
				// Discord APIへの接続失敗（External）として記録。
				logger.Logf("External", "Discord", "接続オープン失敗: %v", err)
				continue
			}

			// スラッシュコマンドを Discord 側へデプロイする。
			m.registerCommands(dg)

			m.mu.Lock()
			m.Sessions[token] = dg
			m.mu.Unlock()
			logger.Logf("Internal", "Discord", "Botを開始しました (token終端: ...%s)", token[len(token)-4:])
		}
	}

	// 設定から除去されたトークンに対応する、古くなった Bot セッションを破棄する。
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
// スラッシュコマンド（/action, /cmd）の中身を定義し、Discord APIを通じて登録する。
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
			logger.Logf("External", "Discord", "コマンド登録失敗 (%s, session=%s): %v", cmd.Name, dg.State.User.ID, err)
		}
	}
}

// MARK: onInteractionCreate()
// スラッシュコマンド実行時のトリガー。ユーザー権限を検証し、許可された場合のみマネージャー経由で処理を叩く。
func (m *BotManager) onInteractionCreate(dg *discordgo.Session, i *discordgo.InteractionCreate) {
	m.mu.RLock()
	serverName, ok := m.ChannelToServer[i.ChannelID]
	m.mu.RUnlock()

	// 呼び出し元のチャンネルが特定の管理対象コンテナに割り当てられていない場合は無視する。
	if !ok {
		return
	}

	cfg := m.Config.Get()
	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}

	// コマンド名から必要な権限を決定する。
	var requiredPerm string
	switch i.ApplicationCommandData().Name {
	case "action":
		requiredPerm = "execute"
	case "cmd":
		requiredPerm = "write"
	default:
		requiredPerm = "read"
	}

	// ユーザー情報と権限リストを照合し、権限のない操作をブロックする。
	allowed := false
	for _, user := range cfg.Users {
		if user.Discord == userID {
			if user.HasPermission(serverName, requiredPerm) {
				allowed = true
			}
			break
		}
	}

	if !allowed {
		// 権限のない操作試行は、クライアント起因の不正アクセス（Client）として記録する。
		logger.Logf("Client", "Discord", "不正アクセス試行: user=%s, target=%s, perm=%s", userID, serverName, requiredPerm)
		dg.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("あなたにはこのサーバーに対する %s 権限がありません。", requiredPerm),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// 長時間の処理（バックアップ等）に備え、一旦レスポンスを保留（Deferred）にする。
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
// 連携チャンネルへの投稿内容を、特定のテンプレートに従ってコンテナの stdin へ自動送信する。
func (m *BotManager) onMessageCreate(dg *discordgo.Session, msg *discordgo.MessageCreate) {
	// Bot自身や他のBotの投稿を無視し、意図しないコマンド連鎖を回避する。
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
	templatePtr := serverCfg.Commands.Message
	if templatePtr == nil || *templatePtr == "" {
		// メッセージ自動送信が設定されていないサーバーの場合は終了。
		return
	}
	template := *templatePtr

	// 投稿者名と本文を埋め込み、コンテナ側のチャット欄等へ反映させる。
	text := strings.ReplaceAll(template, "${user}", msg.Author.Username)
	text = strings.ReplaceAll(text, "${message}", msg.Content)
	docker.SendCommand(serverName, text+"\n")
}

// MARK: interactionErrorEmbed()
// ユーザーへのエラー通知用リッチメッセージを生成する。
func (m *BotManager) interactionErrorEmbed(act string, err error) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color:       colorError,
		Title:       fmt.Sprintf("実行エラー: %s", act),
		Description: err.Error(),
	}
}

// MARK: interactionSuccessEmbed()
// ユーザーへの正常完了通知用リッチメッセージを生成する。
func (m *BotManager) interactionSuccessEmbed(act string, desc string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Color:       colorSuccess,
		Title:       fmt.Sprintf("実行成功: %s", act),
		Description: desc,
	}
}
