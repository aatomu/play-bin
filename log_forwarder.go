package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/docker/docker/api/types/container"
)

// MARK: LogRule
type LogRule struct {
	Comment string         `json:"comment"`
	Regexp  []string       `json:"regexp"`
	Type    string         `json:"type"`
	Webhook WebhookMapping `json:"webhook"`
	res     []*regexp.Regexp
}

type WebhookMapping struct {
	Content   string `json:"content"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url"`
}

// MARK: var
var (
	activeForwarders   = make(map[string]context.CancelFunc) // serverName -> cancel
	forwarderMutex     sync.RWMutex
	logRulesCache      = make(map[string]*logRulesState) // logSettingPath -> state
	logRulesCacheMutex sync.RWMutex
)

type logRulesState struct {
	rules      []LogRule
	lastLoaded time.Time
	mu         sync.RWMutex
}

// MARK: startLogForwarders()
func startLogForwarders() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// 初回実行
	updateLogForwarders()

	for range ticker.C {
		updateLogForwarders()
	}
}

// MARK: updateLogForwarders()
func updateLogForwarders() {
	cfg := config.Get()

	activeServers := make(map[string]bool)

	for serverName, serverCfg := range cfg.Servers {
		if serverCfg.Discord.LogSetting == "" || serverCfg.Discord.Webhook == "" {
			continue
		}
		activeServers[serverName] = true

		forwarderMutex.RLock()
		_, exists := activeForwarders[serverName]
		forwarderMutex.RUnlock()

		if !exists {
			ctx, cancel := context.WithCancel(context.Background())
			forwarderMutex.Lock()
			activeForwarders[serverName] = cancel
			forwarderMutex.Unlock()

			go tailContainerLogs(ctx, serverName, serverCfg.Discord.LogSetting, serverCfg.Discord.Webhook)
			log.Printf("Log forwarder started for %s", serverName)
		}
	}

	// 削除されたサーバーのForwarder停止
	forwarderMutex.Lock()
	for serverName, cancel := range activeForwarders {
		if !activeServers[serverName] {
			cancel()
			delete(activeForwarders, serverName)
			log.Printf("Log forwarder stopped for %s", serverName)
		}
	}
	forwarderMutex.Unlock()
}

// MARK: tailContainerLogs
func tailContainerLogs(ctx context.Context, serverName, logSettingPath, webhookURL string) {
	// Webhook URLからIDとTokenを抽出
	parts := strings.Split(webhookURL, "/")
	if len(parts) < 7 {
		log.Printf("Invalid webhook URL for log forwarding: %s", serverName)
		return
	}
	webhookID := parts[len(parts)-2]
	webhookToken := parts[len(parts)-1]

	containerID := serverName

	options := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "0",
	}

	// リトライ用ループ
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// コンテナの存在確認
		_, err := dockerCli.ContainerInspect(ctx, containerID)
		if err != nil {
			// コンテナが存在しない場合は30秒待って再確認
			time.Sleep(30 * time.Second)
			continue
		}

		reader, err := dockerCli.ContainerLogs(ctx, containerID, options)
		if err != nil {
			log.Printf("Failed to tail logs for %s: %v (retrying in 10s)", serverName, err)
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

			// ルールをホットリロード対応で取得
			rules := getLogRules(logSettingPath)
			if rules == nil {
				continue
			}

			for _, rule := range rules {
				for _, re := range rule.res {
					if re.MatchString(line) {
						matches := re.FindStringSubmatch(line)
						content := rule.Webhook.Content
						username := rule.Webhook.Username
						avatarURL := rule.Webhook.AvatarURL

						// $1, $2, ... を置換
						for i, match := range matches {
							placeholder := fmt.Sprintf("$%d", i)
							content = strings.ReplaceAll(content, placeholder, match)
							username = strings.ReplaceAll(username, placeholder, match)
							avatarURL = strings.ReplaceAll(avatarURL, placeholder, match)
						}

						executeWebhook(webhookID, webhookToken, username, content, avatarURL)
						break // 最初にマッチしたら終了
					}
				}
			}
		}

		reader.Close()
		time.Sleep(5 * time.Second)
	}
}

// MARK: getLogRules
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

	// ファイル更新チェック
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}

	state.mu.RLock()
	needsReload := info.ModTime().After(state.lastLoaded)
	state.mu.RUnlock()

	if needsReload {
		rules, err := loadLogRules(path)
		if err != nil {
			log.Printf("Failed to reload log rules from %s: %v", path, err)
			state.mu.RLock()
			defer state.mu.RUnlock()
			return state.rules
		}

		state.mu.Lock()
		state.rules = rules
		state.lastLoaded = info.ModTime()
		state.mu.Unlock()
		log.Printf("Log rules reloaded from %s", path)
	}

	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.rules
}

// MARK: loadLogRules
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

	// コンパイル
	for i := range rules {
		for _, pattern := range rules[i].Regexp {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid regex %s: %v", pattern, err)
			}
			rules[i].res = append(rules[i].res, re)
		}
	}

	return rules, nil
}

// MARK: executeWebhook
func executeWebhook(webhookID, webhookToken, username, content, avatarURL string) {
	session, _ := discordgo.New("")

	params := &discordgo.WebhookParams{
		Content:   content,
		Username:  username,
		AvatarURL: avatarURL,
	}

	_, err := session.WebhookExecute(webhookID, webhookToken, true, params)
	if err != nil {
		log.Printf("Failed to execute webhook: %v", err)
	}
}
