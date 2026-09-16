package onebot

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

func frame(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func groupMessageFrame(t *testing.T, userID int64, segs []map[string]any) []byte {
	return frame(t, map[string]any{
		"post_type": "message", "time": 1737091282, "self_id": 10000,
		"message_type": "group", "message_id": 1, "user_id": userID,
		"group_id": 123, "raw_message": "raw",
		"sender":  map[string]any{"user_id": userID, "nickname": "小明", "card": "名片名"},
		"message": segs,
	})
}

func TestTranslateGroupMessageBasics(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	raw := groupMessageFrame(t, 10001, []map[string]any{
		{"type": "text", "data": map[string]any{"text": "今天天气"}},
	})
	trr, err := tr.TranslateMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if trr == nil {
		t.Fatal("普通群消息不应被当自回声丢弃")
	}
	if trr.Space.Type != envelope.SpaceGroup || trr.Space.ID != "123" {
		t.Fatalf("空间坐标不符: %+v", trr.Space)
	}
	// 群名片 (card) 优先于昵称
	if trr.Event.Actor.DisplayName != "名片名" {
		t.Fatalf("显示名不符: %q", trr.Event.Actor.DisplayName)
	}
	if trr.Event.Text() != "今天天气" {
		t.Fatalf("文本不符: %q", trr.Event.Text())
	}
	if trr.Event.TS != 1737091282000 {
		t.Fatalf("平台时间不符: %d", trr.Event.TS)
	}
}

func TestTranslateAtSelfAndOthers(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	raw := groupMessageFrame(t, 10001, []map[string]any{
		{"type": "at", "data": map[string]any{"qq": "10001"}},
		{"type": "at", "data": map[string]any{"qq": "10000"}},
		{"type": "text", "data": map[string]any{"text": "在吗"}},
	})
	trr, err := tr.TranslateMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !trr.AtSelf {
		t.Fatal("@bot 应置 AtSelf")
	}
	if got := trr.Event.Text(); !strings.Contains(got, "[@我]") || !strings.Contains(got, "在吗") {
		t.Fatalf("at 段翻译不符: %q", got)
	}
}

func TestTranslateReplySegment(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	raw := groupMessageFrame(t, 10001, []map[string]any{
		{"type": "reply", "data": map[string]any{"id": "555"}},
		{"type": "text", "data": map[string]any{"text": "哦"}},
	})
	trr, err := tr.TranslateMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if trr.Event.ReplyTo != "555" {
		t.Fatalf("reply_to 不符: %q", trr.Event.ReplyTo)
	}
	// reply 段只提取引用 ID, 不产生内容占位 (引用文本由触发管线还原)
	if trr.Event.Text() != "哦" {
		t.Fatalf("reply 段不应生成文本: %q", trr.Event.Text())
	}
}

func TestTranslateSelfEchoDropped(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	// user_id == self_id 的 message
	selfMsg := groupMessageFrame(t, 10000, []map[string]any{
		{"type": "text", "data": map[string]any{"text": "自言自语"}},
	})
	if trr, err := tr.TranslateMessage(selfMsg); err != nil || trr != nil {
		t.Fatalf("自回声应返回 nil, got %v, %v", trr, err)
	}
	// post_type=message_sent
	frame2 := map[string]any{
		"post_type": "message_sent", "time": 1, "self_id": 10000,
		"message_type": "group", "message_id": 2, "user_id": 10000,
		"group_id": 123, "message": []map[string]any{},
	}
	if trr, err := tr.TranslateMessage(frame(t, frame2)); err != nil || trr != nil {
		t.Fatalf("message_sent 应返回 nil, got %v, %v", trr, err)
	}
}

func TestTranslateImageURL(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	raw := groupMessageFrame(t, 10001, []map[string]any{
		{"type": "image", "data": map[string]any{"url": "https://gchat.qpic.cn/x.jpg"}},
	})
	trr, err := tr.TranslateMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	nonText := trr.Event.NonTextParts()
	if len(nonText) != 1 || nonText[0].ImageURL.URL != "https://gchat.qpic.cn/x.jpg" {
		t.Fatalf("图片 part 不符: %+v", trr.Event.Content)
	}
}

func pokeFrame(t *testing.T, subType string, userID, targetID int64) []byte {
	return frame(t, map[string]any{
		"post_type": "notice", "time": 1737091282, "self_id": 10000,
		"notice_type": "notify", "sub_type": subType,
		"group_id": 123, "user_id": userID, "target_id": targetID,
	})
}

func TestPokeByTargetSelf(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	ev, space, pokeSelf, ok := tr.TranslateNotice(pokeFrame(t, "poke", 10001, 10000))
	if !ok || !pokeSelf {
		t.Fatalf("target=bot 的戳一戳应识别为被戳, ok=%v pokeSelf=%v", ok, pokeSelf)
	}
	if ev.Kind != envelope.KindNotice || space.Type != envelope.SpaceGroup || space.ID != "123" {
		t.Fatalf("notice 事件/空间不符: %+v %+v", ev, space)
	}
	if !strings.Contains(ev.Text(), "戳了戳我") {
		t.Fatalf("占位文本不符: %q", ev.Text())
	}
}

func TestPokeGroupPokeVariant(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	_, _, pokeSelf, ok := tr.TranslateNotice(pokeFrame(t, "group_poke", 10001, 10000))
	if !ok || !pokeSelf {
		t.Fatalf("sub_type=group_poke 变体应同样识别, ok=%v pokeSelf=%v", ok, pokeSelf)
	}
}

func TestPokeSwappedFieldSemantics(t *testing.T) {
	// 部分实现的群戳一戳 user_id/target_id 语义相反: user_id=bot, target_id=发起者.
	// 旧代码单侧判定会静默漏掉 — 双侧判定必须命中.
	tr := &Translator{SelfID: "10000"}
	ev, _, pokeSelf, ok := tr.TranslateNotice(pokeFrame(t, "poke", 10000, 10001))
	if !ok || !pokeSelf {
		t.Fatalf("字段语义相反的戳一戳应识别为被戳, ok=%v pokeSelf=%v", ok, pokeSelf)
	}
	if !strings.Contains(ev.Text(), "戳了戳我") {
		t.Fatalf("占位文本不符: %q", ev.Text())
	}
}

func TestPokeOtherTargetNotSelf(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	_, _, pokeSelf, ok := tr.TranslateNotice(pokeFrame(t, "poke", 10001, 10002))
	if !ok || pokeSelf {
		t.Fatalf("戳别人不应触发, ok=%v pokeSelf=%v", ok, pokeSelf)
	}
}

func TestUnknownNoticeDropped(t *testing.T) {
	tr := &Translator{SelfID: "10000"}
	raw := frame(t, map[string]any{
		"post_type": "notice", "time": 1, "self_id": 10000,
		"notice_type": "group_ban", "sub_type": "ban",
		"group_id": 123, "user_id": 10001, "target_id": 10002,
	})
	_, _, _, ok := tr.TranslateNotice(raw)
	if ok {
		t.Fatal("未识别的 notice 应返回 ok=false")
	}
}
