package discord

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
	ctypes "github.com/docker/docker/api/types/container"
	"github.com/play-bin/internal/docker"
)

// MARK: LogRule
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
func (m *BotManager) SyncLogForwarders() {
	cfg := m.Config.Get()
	activeServers := make(map[string]bool)

	for serverName, serverCfg := range cfg.Servers {
		if serverCfg.Discord.LogSetting == "" || serverCfg.Discord.Webhook == "" {
			continue
		}
		activeServers[serverName] = true

		m.ForwarderMu.RLock()
		_, exists := m.ActiveForwarders[serverName]
		m.ForwarderMu.RUnlock()

		if !exists {
			ctx, cancel := context.WithCancel(context.Background())
			m.ForwarderMu.Lock()
			m.ActiveForwarders[serverName] = cancel
			m.ForwarderMu.Unlock()

			go m.tailContainerLogs(ctx, serverName, serverCfg.Discord.LogSetting, serverCfg.Discord.Webhook)
			log.Printf("Log forwarder started for %s", serverName)
		}
	}

	m.ForwarderMu.Lock()
	for serverName, cancel := range m.ActiveForwarders {
		if !activeServers[serverName] {
			cancel()
			delete(m.ActiveForwarders, serverName)
			log.Printf("Log forwarder stopped for %s", serverName)
		}
	}
	m.ForwarderMu.Unlock()
}

func (m *BotManager) tailContainerLogs(ctx context.Context, serverName, logSettingPath, webhookURL string) {
	parts := strings.Split(webhookURL, "/")
	if len(parts) < 7 {
		log.Printf("Invalid webhook URL for log forwarding: %s", serverName)
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

		_, err := docker.Client.ContainerInspect(ctx, serverName)
		if err != nil {
			time.Sleep(30 * time.Second)
			continue
		}

		reader, err := docker.Client.ContainerLogs(ctx, serverName, options)
		if err != nil {
			log.Printf("Failed to tail logs for %s: %v", serverName, err)
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
	needsReload := info.ModTime().After(state.lastLoaded)
	state.mu.RUnlock()

	if needsReload {
		rules, err := loadLogRules(path)
		if err != nil {
			log.Printf("Failed to reload log rules: %v", err)
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

func (m *BotManager) executeWebhook(id, token, user, content, avatar string) {
	dg, _ := discordgo.New("")
	p := &discordgo.WebhookParams{
		Content:   content,
		Username:  user,
		AvatarURL: avatar,
	}
	_, err := dg.WebhookExecute(id, token, true, p)
	if err != nil {
		log.Printf("Webhook failed: %v", err)
	}
}
