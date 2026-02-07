package discord

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

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
		// LogSetting または Webhook が空の場合は、転送を意図していないと判断してスキップする。
		if serverCfg.Discord.LogSetting == "" || serverCfg.Discord.Webhook == "" {
			continue
		}
		activeServers[serverName] = true

		m.ForwarderMu.RLock()
		_, exists := m.ActiveForwarders[serverName]
		m.ForwarderMu.RUnlock()

		if !exists {
			// 新たに監視対象となったコンテナに対して、追跡用の単一ゴルーチンを生成する。
			ctx, cancel := context.WithCancel(context.Background())
			m.ForwarderMu.Lock()
			m.ActiveForwarders[serverName] = cancel
			m.ForwarderMu.Unlock()

			go m.tailContainerLogs(ctx, serverName, serverCfg.Discord.LogSetting, serverCfg.Discord.Webhook)
			logger.Logf("Internal", "Discord", "ログ転送を開始しました: %s", serverName)
		}
	}

	// 構成解除されたサーバーの転送プロセスを、コンテキストを通じて安全に終了させる。
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
// Dockerコンテナのストリームログを監視し、マッチした行を逐次 Webhook へ転送する常駐処理。
func (m *BotManager) tailContainerLogs(ctx context.Context, serverName, logSettingPath, webhookURL string) {
	options := ctypes.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "0", // 接続時点以降の新規ログのみを対象とする
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// コンテナが生存しているか確認。停止中や生成前であれば、リソース保護のため待機を挟む。
		_, err := docker.Client.ContainerInspect(ctx, serverName)
		if err != nil {
			time.Sleep(30 * time.Second)
			continue
		}

		// ログストリームを取得する。
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
			// 各行に対し、最新のフィルタ設定を適用して転送可否を判定する。
			rules := getLogRules(logSettingPath)
			if rules == nil {
				continue
			}

			for _, rule := range rules {
				for _, re := range rule.Res {
					if re.MatchString(line) {
						// マッチした場合、正規表現のキャプチャを活用した置換処理を行い、メッセージを構築する。
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

						m.executeWebhook(webhookURL, username, content, avatarURL)
						break
					}
				}
			}
		}
		reader.Close()
		// ストリームが途絶えた（コンテナ停止等）場合は、再試行まで猶予を持たせる。
		time.Sleep(5 * time.Second)
	}
}

// MARK: getLogRules()
// JSON 形式のログルールを読み込み、コンパイル済みの正規表現をキャッシュして高速に提供する。
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
	// 設定ファイル自体のタイムスタンプを監視し、変更時のみリロードを行う。
	needsReload := info.ModTime().After(state.lastLoaded)
	state.mu.RUnlock()

	if needsReload {
		rules, err := loadLogRules(path)
		if err != nil {
			// ロード失敗時は、可用性を考慮し、前回ロード済みのキャッシュを再利用する。
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
// ファイルからログルールをデコードし、正規表現をメモリ上で高速化するために事前コンパイルする。
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
				// 正規表現の文法エラーは致命的なため、上位に伝播させる。
				return nil, err
			}
			rules[i].Res = append(rules[i].Res, re)
		}
	}
	return rules, nil
}

// MARK: executeWebhook()
// 生成されたペイロードを Discord Webhook API へ POST し、ログ内容をチャンネルへ配信する。
func (m *BotManager) executeWebhook(webhookURL, user, content, avatar string) {
	payload := map[string]string{
		"content":    content,
		"username":   user,
		"avatar_url": avatar,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		logger.Logf("Internal", "Discord", "Webhookペイロードの生成に失敗: %v", err)
		return
	}

	// 外部 API との通信。非同期実行が望ましいが、順序性を考慮しストリームスキャンと同スレッドで行う。
	resp, err := http.Post(webhookURL, "application/json", strings.NewReader(string(jsonData)))
	if err != nil {
		// ネットワーク障害等は外部要因（External）として記録する。
		logger.Logf("External", "Discord", "Webhook送信失敗: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Logf("External", "Discord", "Webhook送信エラー (HTTP %d)", resp.StatusCode)
	}
}
