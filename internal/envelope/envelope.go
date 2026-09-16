// Package envelope 定义 MnemoBot 与 mnemosync 之间的 Envelope v1 协议结构.
//
// 协议权威: docs/design.md §4 与 schemas/envelope-v1.schema.json.
// 平台与协议分离 (设计文档 §4.6): platform 是底层社交平台, protocol 是传输实现;
// 空间坐标以原始值传递, space_id / actor 复合键由 mnemosync 服务端统一组合.
package envelope

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Version 当前实现的协议版本.
const Version = 1

// origin 取值.
const (
	OriginUser      = "user"
	OriginProactive = "persona_proactive"
)

// 事件 kind 取值.
const (
	KindMessage = "message"
	KindNotice  = "notice"
)

// space type 取值.
const (
	SpaceGroup   = "group"
	SpacePrivate = "private"
)

// 协议标识 (protocol 字段; 一期仅 OneBot v11).
const ProtocolOneBot11 = "onebot11"

const (
	EnvelopeBlockOpen  = "<mnemo-envelope>"
	EnvelopeBlockClose = "</mnemo-envelope>"
)

// Space 原始空间坐标: 平台原生 type/id, 不含命名组合规则.
type Space struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Key bot 侧的空间去重键 (与命名组合规则无关).
func (s Space) Key() string { return s.Type + ":" + s.ID }

// Actor 平台原生身份; 服务端将其组合为 "{platform}:{external_key}" 复合键.
type Actor struct {
	ExternalKey string `json:"external_key"`
	DisplayName string `json:"display_name,omitempty"`
}

// ImageURL 图片 content part 的载体.
type ImageURL struct {
	URL string `json:"url"`
}

// Part 对齐 OpenAI content part; 未知类型仅保留 type, 由服务端占位降级.
type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

func TextPart(text string) Part { return Part{Type: "text", Text: text} }

func ImagePart(url string) Part {
	return Part{Type: "image_url", ImageURL: &ImageURL{URL: url}}
}

// Meta 可选提示位, 服务端不做行为分叉 (设计文档 §5.5).
type Meta struct {
	IsBot         *bool  `json:"is_bot,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Event 单条规范化事件.
type Event struct {
	ID      string `json:"id"`
	TS      int64  `json:"ts"` // epoch 毫秒, 平台时间 (排序真相)
	Kind    string `json:"kind,omitempty"`
	Actor   *Actor `json:"actor,omitempty"`
	Content []Part `json:"content"`
	ReplyTo string `json:"reply_to,omitempty"`
	Meta    *Meta  `json:"meta,omitempty"`
}

// Text 拼接全部 text part; 无 text part 返回空串.
func (e *Event) Text() string {
	var sb strings.Builder
	for _, p := range e.Content {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// NonTextParts 返回非文本 parts (图片等), 供触发请求原样携带.
func (e *Event) NonTextParts() []Part {
	out := make([]Part, 0, len(e.Content))
	for _, p := range e.Content {
		if p.Type != "text" {
			out = append(out, p)
		}
	}
	return out
}

// BatchRequest 批量事件端点请求体 (POST /v1/conversation/events).
type BatchRequest struct {
	Version  int     `json:"version"`
	Protocol string  `json:"protocol"`
	Platform string  `json:"platform"`
	Space    Space   `json:"space"`
	Events   []Event `json:"events"`
}

// EventResult 批量端点按事件粒度的处理结果.
type EventResult struct {
	Index  int    `json:"index"`
	Status string `json:"status"` // accepted | rejected
	Reason string `json:"reason,omitempty"`
}

// BatchResponse 批量端点响应体.
type BatchResponse struct {
	Results []EventResult `json:"results"`
}

// Quoted 触发请求携带的被引用消息 (adapter 还原, §5.3).
type Quoted struct {
	Speaker string `json:"speaker,omitempty"`
	Text    string `json:"text,omitempty"`
}

// TriggerEnvelope 触发请求信封, 以 <mnemo-envelope> 块嵌入最后一条 user 消息.
type TriggerEnvelope struct {
	Version  int     `json:"version"`
	Protocol string  `json:"protocol"`
	Platform string  `json:"platform"`
	Space    Space   `json:"space"`
	Event    Event   `json:"event"`
	Origin   string  `json:"origin"`
	Quoted   *Quoted `json:"quoted,omitempty"`
}

// Block 渲染为嵌入消息的信封块.
func (t *TriggerEnvelope) Block() (string, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("序列化信封: %w", err)
	}
	return EnvelopeBlockOpen + string(data) + EnvelopeBlockClose, nil
}

// ActorName 触发事件来源的显示名 (诊断回显用).
func (t *TriggerEnvelope) ActorName() string {
	if t.Event.Actor == nil {
		return "未知"
	}
	if t.Event.Actor.DisplayName != "" {
		return t.Event.Actor.DisplayName
	}
	return t.Event.Actor.ExternalKey
}

// NewCorrelationID 生成随机关联标识, 贯穿 bot→mnemosync 的日志链路.
func NewCorrelationID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
