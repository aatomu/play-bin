package discord

import (
	"bufio"
	"bytes"
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
	Regexp  []string         `json:"regexp"`
	Webhook []map[string]any `json:"webhook"`
	Res     []*regexp.Regexp
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

						// JSONで定義された複数のWebhookメッセージを順次処理
						for _, rawPayload := range rule.Webhook {
							// プレースホルダーの置換を再帰的に実行
							payload := replacePlaceholders(rawPayload, matches)
							m.sendWebhook(webhookURL, payload)
						}
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

// replacePlaceholders は map や slice 内の文字列にある $1, $2 などを再帰的に置換します。
func replacePlaceholders(data any, matches []string) any {
	switch v := data.(type) {
	case string:
		for i, match := range matches {
			placeholder := fmt.Sprintf("$%d", i)
			v = strings.ReplaceAll(v, placeholder, match)
		}
		return v
	case map[string]any:
		newMap := make(map[string]any)
		for k, val := range v {
			newMap[k] = replacePlaceholders(val, matches)
		}
		return newMap
	case []any:
		newSlice := make([]any, len(v))
		for i, val := range v {
			newSlice[i] = replacePlaceholders(val, matches)
		}
		return newSlice
	default:
		return v
	}
}

func (m *BotManager) executeWebhook(webhook string, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		logger.Logf("Internal", "Discord", "Webhook JSON変換失敗: %v", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, webhook, bytes.NewBuffer(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	// 非同期で送るか、タイムアウトを設定したクライアントを推奨
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}
