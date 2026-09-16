// OneBot v11 消息段解析与 envelope 事件翻译.
package onebot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

// Segment OneBot v11 消息段.
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data,omitempty"`
}

// NewTextSegment 构造文本段 (动作用).
func NewTextSegment(text string) Segment {
	return Segment{Type: "text", Data: map[string]any{"text": text}}
}

type senderInfo struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Card     string `json:"card"`
}

// messageEvent OneBot v11 message 事件 (post_type=message / message_sent).
type messageEvent struct {
	Time        int64           `json:"time"`
	SelfID      int64           `json:"self_id"`
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	MessageID   int64           `json:"message_id"`
	UserID      int64           `json:"user_id"`
	GroupID     int64           `json:"group_id"`
	RawMessage  string          `json:"raw_message"`
	Sender      *senderInfo     `json:"sender"`
	Message     json.RawMessage `json:"message"`
}

// noticeEvent OneBot v11 notice 事件.
type noticeEvent struct {
	Time       int64  `json:"time"`
	SelfID     int64  `json:"self_id"`
	NoticeType string `json:"notice_type"`
	SubType    string `json:"sub_type"`
	GroupID    int64  `json:"group_id"`
	UserID     int64  `json:"user_id"`
	TargetID   int64  `json:"target_id"`
}

// Translated 翻译产物: envelope 事件 + 空间坐标 + 触发信号.
type Translated struct {
	Event  envelope.Event
	Space  envelope.Space
	AtSelf bool // 消息 @ 了 bot
}

// Translator v11 → envelope 的翻译器.
type Translator struct {
	SelfID      string
	Platform    string
	Protocol    string
	Base64Media bool
}

// TranslateMessage 将 v11 message 事件翻译为 envelope 事件.
// 自回声 (user_id == self_id, 含 post_type=message_sent) 返回 nil —
// persona 回复已在 /v1 调用时落库, 不得二次落库.
func (t *Translator) TranslateMessage(raw []byte) (*Translated, error) {
	var msg messageEvent
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("解析 message 事件: %w", err)
	}
	if msg.PostType != "message" && msg.PostType != "message_sent" {
		return nil, nil
	}
	if t.selfEcho(msg.UserID) {
		return nil, nil
	}

	senderName := ""
	if msg.Sender != nil {
		senderName = msg.Sender.Card
		if senderName == "" {
			senderName = msg.Sender.Nickname
		}
	}
	if senderName == "" {
		senderName = strconv.FormatInt(msg.UserID, 10)
	}

	// v11 的 message 字段兼容数组与 CQ 字符串两种形态
	var segs []Segment
	if len(msg.Message) > 0 {
		if err := json.Unmarshal(msg.Message, &segs); err != nil {
			// 数组解析失败 → 退回 raw_message (CQ 码由服务端占位降级)
			segs = []Segment{NewTextSegment(msg.RawMessage)}
		}
	} else {
		segs = []Segment{NewTextSegment(msg.RawMessage)}
	}

	parts, atSelf, replyTo := t.translateSegments(segs)

	var space envelope.Space
	if msg.MessageType == "group" {
		space = envelope.Space{Type: envelope.SpaceGroup, ID: fmtID(msg.GroupID)}
	} else {
		space = envelope.Space{Type: envelope.SpacePrivate, ID: fmtID(msg.UserID)}
	}

	ev := envelope.Event{
		ID:      strconv.FormatInt(msg.MessageID, 10),
		TS:      msg.Time * 1000,
		Kind:    envelope.KindMessage,
		Actor:   &envelope.Actor{ExternalKey: fmtID(msg.UserID), DisplayName: senderName},
		Content: parts,
		ReplyTo: replyTo,
		Meta:    &envelope.Meta{CorrelationID: envelope.NewCorrelationID()},
	}
	return &Translated{Event: ev, Space: space, AtSelf: atSelf}, nil
}

func (t *Translator) selfEcho(userID int64) bool {
	selfID, err := strconv.ParseInt(t.SelfID, 10, 64)
	return err == nil && selfID != 0 && userID == selfID
}

func (t *Translator) translateSegments(segs []Segment) (parts []envelope.Part, atSelf bool, replyTo string) {
	for _, seg := range segs {
		switch seg.Type {
		case "text":
			if text := segStr(seg.Data, "text"); text != "" {
				parts = append(parts, envelope.TextPart(text))
			}
		case "face":
			parts = append(parts, envelope.TextPart("[表情]"))
		case "at":
			if segStr(seg.Data, "qq") == t.SelfID {
				atSelf = true
				parts = append(parts, envelope.TextPart("[@我]"))
			} else {
				parts = append(parts, envelope.TextPart("[@某人]"))
			}
		case "reply":
			replyTo = segStr(seg.Data, "id")
		case "image":
			parts = append(parts, t.image(seg))
		case "record":
			parts = append(parts, envelope.TextPart("[语音]"))
		case "video":
			parts = append(parts, envelope.TextPart("[视频]"))
		case "forward":
			parts = append(parts, envelope.TextPart("[合并转发]"))
		case "json", "xml":
			parts = append(parts, envelope.TextPart("[卡片消息]"))
		default:
			parts = append(parts, envelope.TextPart("[未知消息类型:"+seg.Type+"]"))
		}
	}
	if len(parts) == 0 {
		parts = []envelope.Part{envelope.TextPart("[空消息]")}
	}
	return parts, atSelf, replyTo
}

// image 按 §5.6 决策表: URL 优先 → 本地文件 base64 兜底 (可配) → 占位符.
func (t *Translator) image(seg Segment) envelope.Part {
	if url := segStr(seg.Data, "url"); isHTTP(url) {
		return envelope.ImagePart(url)
	}
	if file := segStr(seg.Data, "file"); isHTTP(file) {
		return envelope.ImagePart(file)
	}
	if t.Base64Media {
		if path := segStr(seg.Data, "file"); path != "" {
			if dataURL, err := localFileDataURL(path); err == nil {
				return envelope.ImagePart(dataURL)
			}
		}
	}
	return envelope.TextPart("[图片]")
}

// TranslateNotice 将 v11 notice 翻译为占位文本事件 (§5.5).
// 未识别的 notice 返回 ok=false (不落库, 避免噪音).
func (t *Translator) TranslateNotice(raw json.RawMessage) (ev *envelope.Event, space envelope.Space, pokeSelf bool, ok bool) {
	var n noticeEvent
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, envelope.Space{}, false, false
	}
	var text string
	switch {
	case n.NoticeType == "notify" && n.SubType == "poke":
		if t.selfEcho(n.TargetID) {
			text = "[戳一戳] 用户" + fmtID(n.UserID) + " 戳了戳我"
			pokeSelf = true
		} else {
			text = "[戳一戳] 用户" + fmtID(n.UserID) + " 戳了戳用户" + fmtID(n.TargetID)
		}
	case n.NoticeType == "group_increase":
		text = "[入群] 用户" + fmtID(n.UserID) + " 加入了本群"
	case n.NoticeType == "group_decrease":
		text = "[退群] 用户" + fmtID(n.UserID) + " 离开了本群"
	default:
		return nil, envelope.Space{}, false, false
	}

	if n.GroupID != 0 {
		space = envelope.Space{Type: envelope.SpaceGroup, ID: fmtID(n.GroupID)}
	} else if n.UserID != 0 {
		space = envelope.Space{Type: envelope.SpacePrivate, ID: fmtID(n.UserID)}
	} else {
		return nil, envelope.Space{}, false, false
	}

	ev = &envelope.Event{
		ID:      fmt.Sprintf("notice:%d:%s:%d:%d", n.Time, n.NoticeType, n.UserID, n.TargetID),
		TS:      n.Time * 1000,
		Kind:    envelope.KindNotice,
		Actor:   &envelope.Actor{ExternalKey: fmtID(n.UserID)},
		Content: []envelope.Part{envelope.TextPart(text)},
		Meta:    &envelope.Meta{CorrelationID: envelope.NewCorrelationID()},
	}
	return ev, space, pokeSelf, true
}

var cqCodeRe = regexp.MustCompile(`\[CQ:[^\]]*\]`)

// CleanCQ 将 CQ 码替换为媒体占位符 (引用还原时清理 raw_message).
func CleanCQ(text string) string {
	return cqCodeRe.ReplaceAllString(text, "[媒体]")
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func localFileDataURL(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	mime := "image/jpeg"
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".gif":
		mime = "image/gif"
	case ".webp":
		mime = "image/webp"
	case ".bmp":
		mime = "image/bmp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func segStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func fmtID(v int64) string { return strconv.FormatInt(v, 10) }
