package envelope

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEventTextAndNonTextParts(t *testing.T) {
	ev := Event{
		ID: "1", TS: 1000,
		Content: []Part{TextPart("你好"), ImagePart("http://x/y.jpg"), TextPart("世界")},
	}
	// Text() 拼接全部 text part
	if got := ev.Text(); got != "你好世界" {
		t.Fatalf("Text() = %q, want %q", got, "你好世界")
	}
	nonText := ev.NonTextParts()
	if len(nonText) != 1 || nonText[0].ImageURL == nil || nonText[0].ImageURL.URL != "http://x/y.jpg" {
		t.Fatalf("NonTextParts() 不符预期: %+v", nonText)
	}
}

func TestTriggerBlockRoundTrip(t *testing.T) {
	env := TriggerEnvelope{
		Version: Version, Protocol: ProtocolOneBot11, Platform: "qq",
		Space:  Space{Type: SpaceGroup, ID: "123"},
		Event:  Event{ID: "e1", TS: 1, Content: []Part{TextPart("hi")}},
		Origin: OriginUser,
	}
	block, err := env.Block()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(block, EnvelopeBlockOpen) || !strings.HasSuffix(block, EnvelopeBlockClose) {
		t.Fatalf("信封块标签不符: %s", block)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(block, EnvelopeBlockOpen), EnvelopeBlockClose)
	var parsed TriggerEnvelope
	if err := json.Unmarshal([]byte(inner), &parsed); err != nil {
		t.Fatalf("信封块内 JSON 不可解析: %v", err)
	}
	if !reflect.DeepEqual(parsed, env) {
		t.Fatalf("round-trip 不一致: %+v vs %+v", parsed, env)
	}
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", name))
	if err != nil {
		t.Fatalf("读取夹具 %s: %v", name, err)
	}
	return data
}

// roundTrip 解析→再序列化→再解析, 验证结构体与夹具语义一致 (omitempty 归一).
func roundTrip[A any](t *testing.T, data []byte) A {
	t.Helper()
	var a A
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf("解析夹具: %v", err)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var b A
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenEventText(t *testing.T) {
	ev := roundTrip[Event](t, golden(t, "event_text.json"))
	if ev.ID != "987654" || ev.Kind != KindMessage || ev.Actor.ExternalKey != "10001" {
		t.Fatalf("event_text 夹具解析不符: %+v", ev)
	}
	if ev.Text() != "今天天气怎么样？" {
		t.Fatalf("text part 解析不符: %q", ev.Text())
	}
}

func TestGoldenEventImage(t *testing.T) {
	ev := roundTrip[Event](t, golden(t, "event_image.json"))
	if ev.ReplyTo != "987654" {
		t.Fatalf("reply_to 解析不符: %q", ev.ReplyTo)
	}
	if len(ev.NonTextParts()) != 1 || ev.NonTextParts()[0].ImageURL.URL != "https://gchat.qpic.cn/example.jpg" {
		t.Fatalf("image part 解析不符: %+v", ev.Content)
	}
}

func TestGoldenBatchRequest(t *testing.T) {
	br := roundTrip[BatchRequest](t, golden(t, "batch_request.json"))
	if br.Version != Version || br.Protocol != ProtocolOneBot11 || br.Platform != "qq" {
		t.Fatalf("batch 头部解析不符: %+v", br)
	}
	if br.Space.Type != SpaceGroup || br.Space.ID != "123456" || len(br.Events) != 2 {
		t.Fatalf("batch 夹具解析不符: %+v", br)
	}
}

func TestGoldenTriggerUser(t *testing.T) {
	env := roundTrip[TriggerEnvelope](t, golden(t, "trigger_user.json"))
	if env.Origin != OriginUser || env.Quoted == nil || env.Quoted.Speaker != "小明" {
		t.Fatalf("trigger_user 解析不符: %+v", env)
	}
	if env.Space.Type != SpaceGroup || len(env.Event.Content) != 2 {
		t.Fatalf("trigger_user 事件解析不符: %+v", env.Event)
	}
}

func TestGoldenTriggerProactive(t *testing.T) {
	env := roundTrip[TriggerEnvelope](t, golden(t, "trigger_proactive.json"))
	if env.Origin != OriginProactive || env.Quoted != nil {
		t.Fatalf("trigger_proactive 解析不符: %+v", env)
	}
}
