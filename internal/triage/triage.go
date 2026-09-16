// Package triage 实现 MnemoBot 的触发判定 — 发言权归 bot 侧 (设计文档 §5.3).
//
// 判定信号全部来自平台原生信息 (@我、回复我、名字提及、戳一戳), 不依赖服务端;
// 冷却与静默调度也在本包维护.
package triage

import (
	"strings"
	"sync"
	"time"
)

// Decision 触发判定结果.
type Decision struct {
	Trigger bool
	Reason  string
}

// Signal 一次判定的输入信号.
type Signal struct {
	SpaceType string // group | private
	AtSelf    bool
	ReplyTo   string
	Text      string
	PokeSelf  bool
}

const sentCap = 512

// Triage 触发判定器, 并发安全.
type Triage struct {
	nicknames     []string // 已归一为小写
	cooldown      time.Duration
	silence       time.Duration // 0 = 禁用主动消息
	privateAlways bool

	mu           sync.Mutex
	lastTrigger  map[string]time.Time
	lastActivity map[string]time.Time
	sentIDs      map[string]struct{}
	sentOrder    []string
}

func New(nicknames []string, cooldown, silence time.Duration, privateAlways bool) *Triage {
	lowered := make([]string, 0, len(nicknames))
	for _, n := range nicknames {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			lowered = append(lowered, n)
		}
	}
	return &Triage{
		nicknames:     lowered,
		cooldown:      cooldown,
		silence:       silence,
		privateAlways: privateAlways,
		lastTrigger:   make(map[string]time.Time),
		lastActivity:  make(map[string]time.Time),
		sentIDs:       make(map[string]struct{}),
	}
}

// MarkActivity 记录空间最后活跃时间 (ambient 消息也算活跃).
func (t *Triage) MarkActivity(spaceKey string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastActivity[spaceKey] = now
}

// MarkSent 记录 bot 自身发出消息的 ID, 供"回复我"判定.
func (t *Triage) MarkSent(messageID string) {
	if messageID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.sentIDs[messageID]; exists {
		return
	}
	t.sentIDs[messageID] = struct{}{}
	t.sentOrder = append(t.sentOrder, messageID)
	if len(t.sentOrder) > sentCap {
		old := t.sentOrder[0]
		t.sentOrder = t.sentOrder[1:]
		delete(t.sentIDs, old)
	}
}

// DueSpaces 返回静默时长已到、可以主动发言的空间键.
func (t *Triage) DueSpaces(now time.Time) []string {
	if t.silence <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var due []string
	for key, last := range t.lastActivity {
		if now.Sub(last) >= t.silence {
			due = append(due, key)
		}
	}
	return due
}

// Evaluate 触发判定; 触发时同时刷新冷却计时.
// 被冷却抑制的信号返回 Reason="冷却中" (供调用方观测), 无信号返回空 Decision.
func (t *Triage) Evaluate(spaceKey string, sig Signal, now time.Time) Decision {
	t.mu.Lock()
	defer t.mu.Unlock()

	if sig.SpaceType == "private" {
		if t.privateAlways {
			return t.trigger(spaceKey, now, "私聊")
		}
		return Decision{Reason: "私聊已禁用"}
	}
	if sig.PokeSelf {
		if t.cool(spaceKey, now) {
			return t.trigger(spaceKey, now, "戳一戳")
		}
		return Decision{Reason: "冷却中"}
	}
	if sig.AtSelf {
		if t.cool(spaceKey, now) {
			return t.trigger(spaceKey, now, "@我")
		}
		return Decision{Reason: "冷却中"}
	}
	if sig.ReplyTo != "" {
		if _, ok := t.sentIDs[sig.ReplyTo]; ok {
			if t.cool(spaceKey, now) {
				return t.trigger(spaceKey, now, "回复我")
			}
			return Decision{Reason: "冷却中"}
		}
	}
	text := strings.ToLower(sig.Text)
	for _, nick := range t.nicknames {
		if nick != "" && strings.Contains(text, nick) {
			if t.cool(spaceKey, now) {
				return t.trigger(spaceKey, now, "名字提及")
			}
			return Decision{Reason: "冷却中"}
		}
	}
	return Decision{}
}

func (t *Triage) cool(spaceKey string, now time.Time) bool {
	if last, ok := t.lastTrigger[spaceKey]; ok && now.Sub(last) < t.cooldown {
		return false
	}
	return true
}

func (t *Triage) trigger(spaceKey string, now time.Time, reason string) Decision {
	t.lastTrigger[spaceKey] = now
	return Decision{Trigger: true, Reason: reason}
}
