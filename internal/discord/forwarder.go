package discord

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	ctypes "github.com/docker/docker/api/types/container"
	"github.com/play-bin/internal/docker"
	"github.com/play-bin/internal/logger"
)

// MARK: LogRule
// コンテナログから特定のパターンを検出し、Webhookへ転送するためのルール定義。
type LogRule struct {
	Comment string         `json:"comment"`
	Regexp  []string       `json:"regexp"`
	Type    string         `json:"type"`
	Webhook WebhookMapping `json:"webhook"`
	Res     []*regexp.Regexp
}

type WebhookMapping struct {
	Content   string `json:"content"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// ログルールの読み込み状態を保持するキャッシュ構造体。
type logRulesState struct {
	rules      []LogRule
	lastLoaded time.Time
	mu         sync.RWMutex
}

var (
	logRulesCache      = make(map[string]*logRulesState)
	logRulesCacheMutex sync.RWMutex
)

// MARK: SyncLogForwarders()
// 設定ファイルの内容に合わせて、各コンテナのログ転送プロセスの起動・停止を同期する。
func (m *BotManager) SyncLogForwarders() {
	cfg := m.Config.Get()
	activeServers := make(map[string]bool)

	for serverName, serverCfg := range cfg.Servers {
		// LogSetting または Webhook が空の場合は転送対象外
		if serverCfg.Discord.LogSetting == "" || serverCfg.Discord.Webhook == "" {
			continue
		}
		activeServers[serverName] = true

		m.ForwarderMu.RLock()
		_, exists := m.ActiveForwarders[serverName]
		m.ForwarderMu.RUnlock()

		if !exists {
			// 新しく転送を開始するコンテナに対してゴルーチンを起動
			ctx, cancel := context.WithCancel(context.Background())
			m.ForwarderMu.Lock()
			m.ActiveForwarders[serverName] = cancel
			m.ForwarderMu.Unlock()

			go m.tailContainerLogs(ctx, serverName, serverCfg.Discord.LogSetting, serverCfg.Discord.Webhook)
			logger.Logf("Internal", "Discord", "ログ転送を開始しました: %s", serverName)
		}
	}

	// 転送対象外になったプロセスの停止
	m.ForwarderMu.Lock()
	for serverName, cancel := range m.ActiveForwarders {
		if !activeServers[serverName] {
			cancel()
			delete(m.ActiveForwarders, serverName)
			logger.Logf("Internal", "Discord", "ログ転送を停止しました: %s", serverName)
		}
	}
	m.ForwarderMu.Unlock()
}

// MARK: tailContainerLogs()
// Dockerコンテナのストリームログを監視し、ルールにマッチした行をDiscord Webhookに送信する。
func (m *BotManager) tailContainerLogs(ctx context.Context, serverName, logSettingPath, webhookURL string) {
	// Webhook URL から ID とトークンを抽出
	parts := strings.Split(webhookURL, "/")
	if len(parts) < 7 {
		logger.Logf("Internal", "Discord", "Webhook URLが不正です (%s): %s", serverName, webhookURL)
		return
	}
	webhookID := parts[len(parts)-2]
	webhookToken := parts[len(parts)-1]

	options := ctypes.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "0",
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// コンテナが存在するかチェック。存在しない場合は待機する
		_, err := docker.Client.ContainerInspect(ctx, serverName)
		if err != nil {
			time.Sleep(30 * time.Second)
			continue
		}

		// ログストリームを取得
		reader, err := docker.Client.ContainerLogs(ctx, serverName, options)
		if err != nil {
			logger.Logf("Internal", "Discord", "ログ取得失敗 (%s): %v", serverName, err)
			time.Sleep(10 * time.Second)
			continue
		}

		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				reader.Close()
				return
			default:
			}

			line := scanner.Text()
			// 1行ごとに転送ルールに合致するかチェック
			rules := getLogRules(logSettingPath)
			if rules == nil {
				continue
			}

			for _, rule := range rules {
				for _, re := range rule.Res {
					if re.MatchString(line) {
						matches := re.FindStringSubmatch(line)
						content := rule.Webhook.Content
						username := rule.Webhook.Username
						avatarURL := rule.Webhook.AvatarURL

						// 正規表現のキャプチャグループ（$1, $2...）を実内容で置換
						for i, match := range matches {
							p := fmt.Sprintf("$%d", i)
							content = strings.ReplaceAll(content, p, match)
							username = strings.ReplaceAll(username, p, match)
							avatarURL = strings.ReplaceAll(avatarURL, p, match)
						}

						m.executeWebhook(webhookID, webhookToken, username, content, avatarURL)
						break
					}
				}
			}
		}
		reader.Close()
		time.Sleep(5 * time.Second)
	}
}

// MARK: getLogRules()
// JSON形式のログ転送定義ファイルを読み込み、正規表現をコンパイルしてキャッシュする。
func getLogRules(path string) []LogRule {
	logRulesCacheMutex.RLock()
	state, exists := logRulesCache[path]
	logRulesCacheMutex.RUnlock()

	if !exists {
		state = &logRulesState{}
		logRulesCacheMutex.Lock()
		logRulesCache[path] = state
		logRulesCacheMutex.Unlock()
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	state.mu.RLock()
	// ファイルの更新時刻をチェックし、変更があればリロードする
	needsReload := info.ModTime().After(state.lastLoaded)
	state.mu.RUnlock()

	if needsReload {
		rules, err := loadLogRules(path)
		if err != nil {
			logger.Logf("Internal", "Discord", "ログルールのパース失敗: %v", err)
			state.mu.RLock()
			defer state.mu.RUnlock()
			return state.rules
		}
		state.mu.Lock()
		state.rules = rules
		state.lastLoaded = info.ModTime()
		state.mu.Unlock()
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.rules
}

// MARK: loadLogRules()
// ファイルからログ転送条件をデコードし、正規表現を事前コンパイルする。
func loadLogRules(path string) ([]LogRule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rules []LogRule
	if err := json.NewDecoder(f).Decode(&rules); err != nil {
		return nil, err
	}

	for i := range rules {
		for _, pat := range rules[i].Regexp {
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, err
			}
			rules[i].Res = append(rules[i].Res, re)
		}
	}
	return rules, nil
}

// MARK: executeWebhook()
// 生成されたメッセージを Discord Webhook 経由で送信する。
func (m *BotManager) executeWebhook(id, token, user, content, avatar string) {
	dg, _ := discordgo.New("")
	p := &discordgo.WebhookParams{
		Content:   content,
		Username:  user,
		AvatarURL: avatar,
	}
	_, err := dg.WebhookExecute(id, token, true, p)
	if err != nil {
		logger.Logf("External", "Discord", "Webhook送信失敗: %v", err)
	}
}
