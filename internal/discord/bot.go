package discord

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/play-bin/internal/config"
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

	// 設定にある各サーバーから、チャンネルIDの紐付けとBotトークンを抽出する。
	for serverName, serverCfg := range cfg.Servers {
		if serverCfg.Discord == nil {
			continue
		}
		// チャンネル指定がある場合は、トークンの有無に関わらず管理対象として紐付ける。
		if channelID := serverCfg.Discord.Channel; channelID != "" {
			newChannelToServer[channelID] = serverName
		}

		token := serverCfg.Discord.Token
		if token != "" {
			activeTokens[token] = true
		}
	}

	m.mu.Lock()
	m.ChannelToServer = newChannelToServer
	m.mu.Unlock()

	// 新たに追加されたトークン、または既存セッションが不健全な場合に再起動を試みる。
	for token := range activeTokens {
		m.mu.RLock()
		session, exists := m.Sessions[token]
		m.mu.RUnlock()

		shouldStart := !exists
		if exists && session == nil {
			shouldStart = true
		}

		if shouldStart {
			dg, err := discordgo.New("Bot " + token)
			if err != nil {
				logger.Logf("External", "Discord", "セッション作成失敗: %v", err)
				continue
			}

			dg.AddHandler(m.onInteractionCreate)
			dg.AddHandler(m.onMessageCreate)

			if err := dg.Open(); err != nil {
				logger.Logf("External", "Discord", "接続オープン失敗 (token終端: ...%s): %v", token[len(token)-4:], err)
				m.mu.Lock()
				m.Sessions[token] = nil // リトライ対象として nil をセット
				m.mu.Unlock()
				continue
			}

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
			if session != nil {
				session.Close()
			}
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
						{Name: "remove", Value: "remove"},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "generation",
					Description: "復元するバックアップ世代（restore時は必須）",
					Required:    false,
				},
			},
		},
		{
			Name:        "backups",
			Description: "バックアップ世代の一覧を表示します",
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
		act := i.ApplicationCommandData().Options[0].StringValue()
		requiredPerm = containerToPerm(container.Action(act))
	case "backups":
		requiredPerm = config.PermContainerRead
	case "cmd":
		requiredPerm = config.PermContainerWrite
	default:
		requiredPerm = config.PermContainerRead
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

		var actionErr error
		if act == "restore" {
			// restore は世代指定が必須。未指定時はエラーを返す。
			generation := ""
			if len(i.ApplicationCommandData().Options) > 1 {
				generation = i.ApplicationCommandData().Options[1].StringValue()
			}
			if generation == "" {
				dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
					Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed(act,
						fmt.Errorf("世代の指定が必要です。/backups で一覧を確認してください"))},
				})
				return
			}
			actionErr = m.ContainerManager.Restore(context.Background(), serverName, generation)
		} else {
			actionErr = m.ContainerManager.ExecuteAction(context.Background(), serverName, container.Action(act))
		}

		if actionErr != nil {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed(act, actionErr)},
			})
			return
		}
		dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{m.interactionSuccessEmbed(act, "実行が完了しました")},
		})
	case "backups":
		// バックアップ世代の一覧を取得し、Embedで表示する。
		generations, err := m.ContainerManager.ListBackupGenerations(serverName)
		if err != nil {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionErrorEmbed("backups", err)},
			})
			return
		}
		if len(generations) == 0 {
			dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Embeds: &[]*discordgo.MessageEmbed{m.interactionSuccessEmbed("backups", "バックアップが見つかりません")},
			})
			return
		}
		// 一覧を見やすく整形して表示する。
		var listText strings.Builder
		for idx, g := range generations {
			if idx >= 20 {
				listText.WriteString(fmt.Sprintf("\n...他 %d 件", len(generations)-20))
				break
			}
			listText.WriteString(fmt.Sprintf("`%s`\n", g))
		}
		dg.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Embeds: &[]*discordgo.MessageEmbed{{
				Color:       colorInfo,
				Title:       fmt.Sprintf("バックアップ一覧: %s", serverName),
				Description: listText.String(),
			}},
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

func containerToPerm(a container.Action) string {
	switch a {
	case container.ActionStart:
		return config.PermContainerStart
	case container.ActionStop:
		return config.PermContainerStop
	case container.ActionKill:
		return config.PermContainerKill
	case container.ActionBackup:
		return config.PermContainerBackup
	case container.ActionRemove:
		return config.PermContainerRemove
	default:
		return config.PermContainerExecute
	}
}
