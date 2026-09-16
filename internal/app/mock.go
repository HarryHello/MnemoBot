// 内置 mock 上游: 不依赖真实 mnemosync 的 bot 接口自测 (serve --mock).
//
// 承接 /v1/conversation/events 与 /v1/chat/completions:
//   - events: 全部接受, 并按事件 ID 记忆"已落库"集合;
//   - chat:   解析触发信封, 校验 flush 纪律 (user 触发事件必须先落库),
//     把解析结果回显成 mock 回复 → 从 QQ 里直接看到 bot 解析到的接口字段.
package app

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

const mockSeenCap = 8192

type mockUpstream struct {
	log *slog.Logger
	url string

	mu   sync.Mutex
	seen map[string]struct{}
}

// startMockUpstream 在 127.0.0.1 随机端口启动 mock 服务.
func startMockUpstream(log *slog.Logger) (*mockUpstream, error) {
	m := &mockUpstream{log: log, seen: make(map[string]struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/conversation/events", m.handleEvents)
	mux.HandleFunc("POST /v1/chat/completions", m.handleChat)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m.url = "http://" + ln.Addr().String()
	go func() {
		_ = (&http.Server{Handler: mux}).Serve(ln)
	}()
	log.Warn("⚠️ MOCK 模式: 不连接真实 mnemosync, 上游由内置 mock 承接", "mock_url", m.url)
	return m, nil
}

func (m *mockUpstream) handleEvents(w http.ResponseWriter, r *http.Request) {
	var br envelope.BatchRequest
	if err := json.NewDecoder(r.Body).Decode(&br); err != nil {
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	if len(m.seen) >= mockSeenCap {
		m.seen = make(map[string]struct{})
	}
	for _, ev := range br.Events {
		m.seen[ev.ID] = struct{}{}
	}
	m.mu.Unlock()

	first := ""
	if len(br.Events) > 0 {
		first = br.Events[0].ID
	}
	m.log.Info("📨 事件落库 (mock)",
		"space", br.Space.Type+":"+br.Space.ID,
		"platform", br.Platform,
		"count", len(br.Events),
		"first", first)

	results := make([]envelope.EventResult, len(br.Events))
	for i := range results {
		results[i] = envelope.EventResult{Index: i, Status: "accepted"}
	}
	_ = json.NewEncoder(w).Encode(envelope.BatchResponse{Results: results})
}

func (m *mockUpstream) handleChat(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var parts []envelope.Part
	for i := len(payload.Messages) - 1; i >= 0; i-- {
		if payload.Messages[i].Role != "user" {
			continue
		}
		if err := json.Unmarshal(payload.Messages[i].Content, &parts); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		break
	}

	env, ok := parseEnvelopeFromParts(parts)
	if !ok {
		text := "（mock 回复）⚠️ 触发请求中未找到 mnemo-envelope 信封块 — 请检查触发管线"
		writeChat(w, text)
		return
	}

	m.mu.Lock()
	_, seenBefore := m.seen[env.Event.ID]
	m.mu.Unlock()

	discipline := "无需落库 (主动消息)"
	switch {
	case env.Origin == envelope.OriginUser && seenBefore:
		discipline = "✅ 触发事件已先落库 (flush 纪律通过)"
	case env.Origin == envelope.OriginUser:
		discipline = "❌ 触发事件未先落库 (违反 flush 纪律)"
	}

	quoted := "无"
	if env.Quoted != nil {
		quoted = fmt.Sprintf("%s:「%s」", env.Quoted.Speaker, runeTruncate(env.Quoted.Text, 40))
	}
	images := len(env.Event.NonTextParts())

	text := fmt.Sprintf(
		"（mock 回复）✅ 链路全通\n触发: %s | 空间: %s:%s | 事件: %s | 来自: %s\n引用: %s | 图片: %d\n落库纪律: %s",
		env.Origin, env.Platform, env.Space.Type+":"+env.Space.ID, env.Event.ID,
		env.ActorName(), quoted, images, discipline,
	)
	writeChat(w, text)
}

// parseEnvelopeFromParts 从 user 消息 parts 中提取并解析信封块.
func parseEnvelopeFromParts(parts []envelope.Part) (envelope.TriggerEnvelope, bool) {
	var env envelope.TriggerEnvelope
	for _, p := range parts {
		if p.Type != "text" {
			continue
		}
		start := strings.Index(p.Text, envelope.EnvelopeBlockOpen)
		if start < 0 {
			continue
		}
		end := strings.Index(p.Text[start:], envelope.EnvelopeBlockClose)
		if end < 0 {
			return env, false
		}
		inner := p.Text[start+len(envelope.EnvelopeBlockOpen) : start+end]
		if err := json.Unmarshal([]byte(inner), &env); err != nil {
			return env, false
		}
		return env, true
	}
	return env, false
}

func writeChat(w http.ResponseWriter, content string) {
	out, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"content": content},
		}},
	})
	_, _ = w.Write(out)
}

func runeTruncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
