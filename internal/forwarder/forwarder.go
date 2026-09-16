// Package forwarder 负责与 mnemosync 的全部 HTTP 通信:
// 批量事件落库 (含触发前 flush 纪律, §5.2) 与 /v1 触发调用 (§4.3).
package forwarder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
	"github.com/HarryHello/mnemo-bot/internal/journal"
)

const (
	flushInterval  = time.Second
	maxBatchSize   = 200
	maxRequeueSize = 500
	maxPostRetries = 3
	retryBaseDelay = 200 * time.Millisecond
)

// Forwarder 上游通信器: 按空间批缓冲事件、定时/按需 flush、触发调用.
type Forwarder struct {
	baseURL   string
	apiKey    string
	model     string
	protocol  string
	platform  string
	jr        *journal.Journal
	log       *slog.Logger
	hc        *http.Client

	flushNow chan chan struct{}
	stopCh   chan struct{}
	doneCh   chan struct{}

	mu      sync.Mutex
	groups  map[string][]envelope.Event
	spaceOf map[string]envelope.Space
	order   []string
}

// New 构造 Forwarder.
func New(baseURL, apiKey, model, protocol, platform string, timeout time.Duration, jr *journal.Journal, log *slog.Logger) *Forwarder {
	return &Forwarder{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		model:    model,
		protocol: protocol,
		platform: platform,
		jr:       jr,
		log:      log,
		hc:       &http.Client{Timeout: timeout},
		flushNow: make(chan chan struct{}, 16),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		groups:   make(map[string][]envelope.Event),
		spaceOf:  make(map[string]envelope.Space),
	}
}

// Enqueue 记录一个事件: 先落 journal 再入批缓冲 (断线补投数据源, §5.7).
func (f *Forwarder) Enqueue(space envelope.Space, ev envelope.Event) {
	f.jr.Append(journal.Entry{
		Kind:   journal.KindEnqueue,
		Space:  space,
		Events: []envelope.Event{ev},
	})
	f.buffer(space, ev)
}

// Restore 重启补投入口: 事件已在 journal 中, 仅入批缓冲, 不再写 journal.
func (f *Forwarder) Restore(space envelope.Space, ev envelope.Event) {
	f.buffer(space, ev)
}

func (f *Forwarder) buffer(space envelope.Space, ev envelope.Event) {
	key := space.Key()
	f.mu.Lock()
	if _, ok := f.spaceOf[key]; !ok {
		f.order = append(f.order, key)
		f.spaceOf[key] = space
	}
	f.groups[key] = append(f.groups[key], ev)
	f.mu.Unlock()
}

// Run 循环: 周期 flush + 响应触发前 flush 请求; Stop 时做最后一轮尽力落库.
func (f *Forwarder) Run() {
	defer close(f.doneCh)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stopCh:
			f.flushRound()
			return
		case <-ticker.C:
			f.flushRound()
		case done := <-f.flushNow:
			f.flushRound()
			close(done)
		}
	}
}

// Stop 结束 flusher, 同步完成最后一轮 flush.
func (f *Forwarder) Stop() {
	close(f.stopCh)
	<-f.doneCh
}

// FlushAll 触发前纪律 (§5.2): 把当前缓冲中的全部批次落库并等确认后才返回.
// 调用方必须先 FlushAll 再发起触发请求, 否则 persona 上下文会缺失触发前的 ambient 消息.
func (f *Forwarder) FlushAll(ctx context.Context) error {
	done := make(chan struct{}, 1)
	select {
	case f.flushNow <- done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Trigger 发起触发请求 (/v1/chat/completions), 返回 persona 回复文本.
func (f *Forwarder) Trigger(ctx context.Context, env envelope.TriggerEnvelope) (string, error) {
	block, err := env.Block()
	if err != nil {
		return "", err
	}
	text := block
	if body := env.Event.Text(); body != "" {
		text += "\n" + body
	}
	parts := append([]envelope.Part{envelope.TextPart(text)}, env.Event.NonTextParts()...)

	payload := map[string]any{
		"model":  f.model,
		"stream": false,
		"messages": []map[string]any{
			{"role": "user", "content": parts},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化触发请求: %w", err)
	}
	status, respBody, err := f.post(ctx, "/v1/chat/completions", body)
	if err != nil {
		return "", fmt.Errorf("调用 /v1: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("/v1 返回 HTTP %d: %s", status, truncate(respBody, 300))
	}
	var chat chatResponse
	if err := json.Unmarshal(respBody, &chat); err != nil {
		return "", fmt.Errorf("解析 /v1 响应: %w", err)
	}
	if len(chat.Choices) == 0 {
		return "", errors.New("/v1 响应无 choices")
	}
	content := chat.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", errors.New("persona 回复为空")
	}
	return content, nil
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// ── 批量上报内部实现 ─────────────────────────────────────

func (f *Forwarder) flushRound() {
	f.mu.Lock()
	if len(f.groups) == 0 {
		f.mu.Unlock()
		return
	}
	type batch struct {
		space  envelope.Space
		events []envelope.Event
	}
	batches := make([]batch, 0, len(f.groups))
	for _, key := range f.order {
		if evs := f.groups[key]; len(evs) > 0 {
			batches = append(batches, batch{space: f.spaceOf[key], events: evs})
		}
	}
	f.groups = make(map[string][]envelope.Event)
	f.spaceOf = make(map[string]envelope.Space)
	f.order = nil
	f.mu.Unlock()

	for _, b := range batches {
		f.postBatch(b.space, b.events)
	}
}

func (f *Forwarder) postBatch(space envelope.Space, events []envelope.Event) {
	if len(events) > maxBatchSize {
		// 超大批次拆分提交 (ID 顺序不变)
		for len(events) > maxBatchSize {
			f.postBatch(space, events[:maxBatchSize])
			events = events[maxBatchSize:]
		}
	}
	if len(events) == 0 {
		return
	}

	req := envelope.BatchRequest{
		Version:  envelope.Version,
		Protocol: f.protocol,
		Platform: f.platform,
		Space:    space,
		Events:   events,
	}
	body, err := json.Marshal(req)
	if err != nil {
		f.log.Error("序列化批次失败 (事件保留 journal)", "err", err)
		f.requeue(space, events)
		return
	}

	var lastErr error
	for attempt := 0; attempt < maxPostRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryBaseDelay * time.Duration(1<<attempt))
		}
		status, respBody, err := f.post(context.Background(), "/v1/conversation/events", body)
		switch {
		case err == nil && status == http.StatusOK:
			f.ackBatch(space, events, respBody)
			return
		case err != nil:
			lastErr = err
		default:
			lastErr = fmt.Errorf("HTTP %d: %s", status, truncate(respBody, 200))
			if status >= 400 && status < 500 {
				// 4xx 是配置/契约级问题, 重试无意义; 终态拒绝记入 journal, 重启后不再重推
				f.log.Error("批量事件被服务端拒绝 (终态)", "status", status, "detail", lastErr, "count", len(events))
				ids := make([]string, len(events))
				for i, ev := range events {
					ids[i] = ev.ID
				}
				_ = f.jr.Append(journal.Entry{Kind: journal.KindAck, Rejected: ids})
				return
			}
		}
	}
	f.log.Error("批量事件上报失败 (保留待下轮/重启补投)", "err", lastErr, "count", len(events))
	f.requeue(space, events)
}

func (f *Forwarder) ackBatch(space envelope.Space, events []envelope.Event, respBody []byte) {
	var br envelope.BatchResponse
	if err := json.Unmarshal(respBody, &br); err != nil {
		f.log.Warn("解析批量响应失败 (按全部成功处理)", "err", err)
		f.writeAck(events, nil)
		return
	}
	rejectedSet := make(map[int]struct{}, len(br.Results))
	for _, r := range br.Results {
		if r.Status == "rejected" {
			rejectedSet[r.Index] = struct{}{}
		}
	}
	var acked, rejected []envelope.Event
	for i, ev := range events {
		if _, bad := rejectedSet[i]; bad {
			rejected = append(rejected, ev)
		} else {
			acked = append(acked, ev)
		}
	}
	f.log.Info("批量事件上报完成",
		"space", space.Type+":"+space.ID,
		"accepted", len(acked), "rejected", len(rejected))
	f.writeAck(acked, rejected)
}

func (f *Forwarder) writeAck(acked, rejected []envelope.Event) {
	ids := func(evs []envelope.Event) []string {
		out := make([]string, 0, len(evs))
		for _, ev := range evs {
			out = append(out, ev.ID)
		}
		return out
	}
	if len(acked) == 0 && len(rejected) == 0 {
		return
	}
	if err := f.jr.Append(journal.Entry{
		Kind:     journal.KindAck,
		Acked:    ids(acked),
		Rejected: ids(rejected),
	}); err != nil {
		f.log.Warn("journal ack 落盘失败", "err", err)
	}
}

// requeue 失败批次放回队头; 超限丢最旧 (幂等补投兜底, §5.7).
func (f *Forwarder) requeue(space envelope.Space, events []envelope.Event) {
	key := space.Key()
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.spaceOf[key]; !ok {
		f.order = append(f.order, key)
		f.spaceOf[key] = space
	}
	combined := make([]envelope.Event, 0, len(events)+len(f.groups[key]))
	combined = append(combined, events...)
	combined = append(combined, f.groups[key]...)
	if len(combined) > maxRequeueSize {
		dropped := len(combined) - maxRequeueSize
		combined = combined[dropped:]
		f.log.Warn("积压超限, 丢弃最旧事件", "dropped", dropped)
	}
	f.groups[key] = combined
}

func (f *Forwarder) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.apiKey)
	resp, err := f.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}

func truncate(s []byte, max int) string {
	if len(s) <= max {
		return string(s)
	}
	return string(s[:max]) + "…"
}
