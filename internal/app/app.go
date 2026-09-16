// Package app 组装 mnemo-bot 各组件并运行主循环.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HarryHello/mnemo-bot/internal/config"
	"github.com/HarryHello/mnemo-bot/internal/deliver"
	"github.com/HarryHello/mnemo-bot/internal/envelope"
	"github.com/HarryHello/mnemo-bot/internal/forwarder"
	"github.com/HarryHello/mnemo-bot/internal/journal"
	"github.com/HarryHello/mnemo-bot/internal/onebot"
	"github.com/HarryHello/mnemo-bot/internal/triage"
)

const (
	dataDir         = "data"
	journalMaxBytes = 8 << 20 // 8 MiB 单文件上限
	triggerTimeout  = 3 * time.Minute
	proactiveTick   = 30 * time.Second
)

// Messenger 是动作出口抽象 (onebot.Server 实现).
type Messenger interface {
	SendGroupMsg(ctx context.Context, groupID int64, text string) (string, error)
	SendPrivateMsg(ctx context.Context, userID int64, text string) (string, error)
	GetMessage(ctx context.Context, messageID string) (json.RawMessage, error)
}

// Pipeline 串联 adapter → triage → forwarder → deliver, 实现 onebot.Handlers.
type Pipeline struct {
	cfg *config.Config
	log *slog.Logger
	fw  *forwarder.Forwarder
	tr  *triage.Triage
	msg Messenger

	spaceMu sync.Mutex
	spaces  map[string]envelope.Space

	busyMu sync.Mutex
	busy   map[string]bool
}

// Run 装配并运行整个运行时, 阻塞到 ctx 结束.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	jr, err := journal.Open(dataDir, journalMaxBytes)
	if err != nil {
		return fmt.Errorf("打开 journal: %w", err)
	}

	baseURL := cfg.Mnemosync.BaseURL
	if cfg.Mnemosync.Mock {
		mock, err := startMockUpstream(log)
		if err != nil {
			return fmt.Errorf("启动 mock 上游: %w", err)
		}
		baseURL = mock.url
	}

	fw := forwarder.New(
		baseURL, cfg.Mnemosync.APIKey, cfg.Mnemosync.Model,
		envelope.ProtocolOneBot11, cfg.Onebot.Platform,
		time.Duration(cfg.Mnemosync.TimeoutSec)*time.Second,
		jr, log,
	)
	go fw.Run()

	tr := triage.New(cfg.Persona.Nicknames,
		time.Duration(cfg.Triage.GroupCooldownSeconds)*time.Second,
		time.Duration(cfg.Triage.SilenceMinutes())*time.Minute,
		cfg.Triage.PrivateAlwaysEnabled())

	p := &Pipeline{
		cfg:    cfg,
		log:    log,
		fw:     fw,
		tr:     tr,
		spaces: make(map[string]envelope.Space),
		busy:   make(map[string]bool),
	}

	if err := p.replayJournal(log); err != nil {
		log.Warn("journal 回放失败 (跳过补投)", "err", err)
	}

	translator := &onebot.Translator{
		SelfID:      cfg.Onebot.SelfID,
		Platform:    cfg.Onebot.Platform,
		Protocol:    envelope.ProtocolOneBot11,
		Base64Media: cfg.Onebot.Base64Media,
	}
	server := onebot.NewServer(cfg.Onebot.Listen, cfg.Onebot.AccessToken, translator, p, log)
	p.msg = server

	go p.proactiveLoop(ctx)

	log.Info("mnemo-bot 启动",
		"platform", cfg.Onebot.Platform,
		"mnemosync", cfg.Mnemosync.BaseURL,
		"proactive", cfg.Triage.SilenceMinutes())

	srvErr := server.Run(ctx)

	// 优雅收尾: 同步完成最后一轮批量落库, 再关 journal
	fw.Stop()
	if err := jr.Close(); err != nil {
		log.Warn("关闭 journal 失败", "err", err)
	}
	return srvErr
}

// replayJournal 重启补投: journal 中未确认事件重新入批 (服务端指纹去重兜底).
func (p *Pipeline) replayJournal(log *slog.Logger) error {
	pending, err := journal.Replay(dataDir)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	log.Info("journal 回放: 补投未确认事件", "count", len(pending))
	for _, item := range pending {
		p.fw.Restore(item.Space, item.Event)
	}
	return nil
}

// ── onebot.Handlers 实现 ─────────────────────────────────

func (p *Pipeline) GroupMessage(ctx context.Context, tr *onebot.Translated) {
	p.record(tr.Space, tr.Event)
	p.tr.MarkActivity(p.spaceKey(tr.Space), time.Now())
	sig := triage.Signal{
		SpaceType: tr.Space.Type,
		AtSelf:    tr.AtSelf,
		ReplyTo:   tr.Event.ReplyTo,
		Text:      tr.Event.Text(),
	}
	p.maybeTrigger(tr.Space, tr.Event, sig, false)
}

func (p *Pipeline) PrivateMessage(ctx context.Context, tr *onebot.Translated) {
	p.record(tr.Space, tr.Event)
	p.tr.MarkActivity(p.spaceKey(tr.Space), time.Now())
	sig := triage.Signal{SpaceType: envelope.SpacePrivate}
	p.maybeTrigger(tr.Space, tr.Event, sig, false)
}

func (p *Pipeline) Notice(ctx context.Context, ev *envelope.Event, space envelope.Space, pokeSelf bool) {
	p.record(space, *ev)
	if !pokeSelf {
		return
	}
	p.tr.MarkActivity(p.spaceKey(space), time.Now())
	sig := triage.Signal{SpaceType: space.Type, PokeSelf: true}
	p.maybeTrigger(space, *ev, sig, false)
}

func (p *Pipeline) spaceKey(space envelope.Space) string { return space.Key() }

func (p *Pipeline) record(space envelope.Space, ev envelope.Event) {
	p.spaceMu.Lock()
	p.spaces[space.Key()] = space
	p.spaceMu.Unlock()
	p.fw.Enqueue(space, ev)
}

// maybeTrigger 判定并启动触发 (每空间单飞: 已有触发进行中则跳过).
func (p *Pipeline) maybeTrigger(space envelope.Space, ev envelope.Event, sig triage.Signal, proactive bool) {
	key := p.spaceKey(space)
	dec := p.tr.Evaluate(key, sig, time.Now())
	if !dec.Trigger {
		return
	}
	if !p.markBusy(key) {
		p.log.Info("已有触发进行中, 跳过本次触发", "space", key, "reason", dec.Reason)
		return
	}
	go p.runTrigger(dec.Reason, space, ev, proactive)
}

func (p *Pipeline) markBusy(key string) bool {
	p.busyMu.Lock()
	defer p.busyMu.Unlock()
	if p.busy[key] {
		return false
	}
	p.busy[key] = true
	return true
}

func (p *Pipeline) clearBusy(key string) {
	p.busyMu.Lock()
	delete(p.busy, key)
	p.busyMu.Unlock()
}

// runTrigger 执行一次完整触发: flush 纪律 → /v1 → 投递.
func (p *Pipeline) runTrigger(reason string, space envelope.Space, ev envelope.Event, proactive bool) {
	key := p.spaceKey(space)
	defer p.clearBusy(key)

	ctx, cancel := context.WithTimeout(context.Background(), triggerTimeout)
	defer cancel()

	var env envelope.TriggerEnvelope
	if proactive {
		// 主动消息 (合成请求式, §5.4): 触发在 bot 侧, 生成在服务端;
		// origin=persona_proactive → 服务端不落 user turn, 回复按 assistant 落库.
		env = envelope.TriggerEnvelope{
			Version:  envelope.Version,
			Protocol: envelope.ProtocolOneBot11,
			Platform: p.cfg.Onebot.Platform,
			Space:    space,
			Event: envelope.Event{
				ID:      "proactive-" + envelope.NewCorrelationID(),
				TS:     time.Now().UnixMilli(),
				Kind:   envelope.KindMessage,
				Content: []envelope.Part{envelope.TextPart(p.cfg.Triage.ProactivePrompt)},
			},
			Origin: envelope.OriginProactive,
		}
	} else {
		env = envelope.TriggerEnvelope{
			Version:  envelope.Version,
			Protocol: envelope.ProtocolOneBot11,
			Platform: p.cfg.Onebot.Platform,
			Space:    space,
			Event:    ev,
			Origin:   envelope.OriginUser,
			Quoted:   p.resolveQuoted(ctx, ev.ReplyTo),
		}
	}

	// 触发前纪律: 缓冲中的 ambient 批次全部落库并确认后才发起触发请求
	if err := p.fw.FlushAll(ctx); err != nil {
		p.log.Warn("触发前 flush 失败, 仍继续触发", "space", key, "err", err)
	}

	reply, err := p.fw.Trigger(ctx, env)
	if err != nil {
		p.log.Warn("触发调用失败", "reason", reason, "space", key, "err", err)
		return
	}

	p.deliverReply(ctx, space, reply)
}

// resolveQuoted 引用还原 (§5.3): 取被引消息的发送者与文本并入触发请求.
func (p *Pipeline) resolveQuoted(ctx context.Context, quoteID string) *envelope.Quoted {
	if quoteID == "" {
		return nil
	}
	raw, err := p.msg.GetMessage(ctx, quoteID)
	if err != nil {
		p.log.Warn("引用还原失败 (无引用上下文继续)", "id", quoteID, "err", err)
		return nil
	}
	var m struct {
		Sender struct {
			Nickname string `json:"nickname"`
			Card     string `json:"card"`
		} `json:"sender"`
		RawMessage string `json:"raw_message"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	text := strings.TrimSpace(onebot.CleanCQ(m.RawMessage))
	if text == "" {
		return nil
	}
	speaker := m.Sender.Card
	if speaker == "" {
		speaker = m.Sender.Nickname
	}
	if speaker == "" {
		speaker = "未知"
	}
	return &envelope.Quoted{Speaker: speaker, Text: text}
}

// deliverReply 投递回复: 超长拆分 + 节奏控制; 记录发出的 message_id.
func (p *Pipeline) deliverReply(ctx context.Context, space envelope.Space, reply string) {
	chunks := deliver.SplitText(reply, p.cfg.Deliver.MaxTextChars)
	delay := time.Duration(p.cfg.Deliver.SendDelayMs) * time.Millisecond
	for i, chunk := range chunks {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		var sentID string
		var err error
		if space.Type == envelope.SpaceGroup {
			var id int64
			id, err = strconv.ParseInt(space.ID, 10, 64)
			if err != nil {
				p.log.Warn("群号解析失败, 放弃投递", "id", space.ID)
				return
			}
			sentID, err = p.msg.SendGroupMsg(ctx, id, chunk)
		} else {
			var id int64
			id, err = strconv.ParseInt(space.ID, 10, 64)
			if err != nil {
				p.log.Warn("用户号解析失败, 放弃投递", "id", space.ID)
				return
			}
			sentID, err = p.msg.SendPrivateMsg(ctx, id, chunk)
		}
		if err != nil {
			// 投递失败是 bot 侧重试责任; 流水里的 assistant 轮不因投递失败回滚 (§5.7)
			p.log.Warn("投递失败", "err", err)
			return
		}
		if sentID != "" {
			p.tr.MarkSent(sentID)
		}
	}
}

// proactiveLoop 主动消息调度 (§5.4): 静默阈值到的空间发起合成请求.
func (p *Pipeline) proactiveLoop(ctx context.Context) {
	ticker := time.NewTicker(proactiveTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		silence := time.Duration(p.cfg.Triage.SilenceMinutes()) * time.Minute
		if silence <= 0 {
			continue
		}
		now := time.Now()
		for _, key := range p.tr.DueSpaces(now) {
			p.spaceMu.Lock()
			space, ok := p.spaces[key]
			p.spaceMu.Unlock()
			if !ok || !p.markBusy(key) {
				continue
			}
			// 触发即重置活跃计时, 避免连环主动
			p.tr.MarkActivity(key, now)
			go p.runTrigger("静默阈值", space, envelope.Event{}, true)
		}
	}
}
