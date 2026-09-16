package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

func TestMockDisciplineAndParsing(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock, err := startMockUpstream(log)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}

	sp := envelope.Space{Type: "group", ID: "123"}
	ev := envelope.Event{ID: "m1", TS: 1, Content: []envelope.Part{envelope.TextPart("你好")}}

	// 1) 触发事件先落库
	if err := postBatch(client, mock.url, envelope.BatchRequest{
		Version: envelope.Version, Protocol: envelope.ProtocolOneBot11, Platform: "qq",
		Space: sp, Events: []envelope.Event{ev},
	}); err != nil {
		t.Fatalf("events 上报: %v", err)
	}

	// 2) user 触发 → mock 应确认 flush 纪律通过并回显解析字段
	env := envelope.TriggerEnvelope{
		Version: envelope.Version, Protocol: envelope.ProtocolOneBot11, Platform: "qq",
		Space:  sp,
		Event:  ev,
		Origin: envelope.OriginUser,
		Quoted: &envelope.Quoted{Speaker: "小明", Text: "上一句"},
	}
	text := chatText(t, client, mock.url, env)
	for _, want := range []string{"✅ 链路全通", "已先落库", "小明:「上一句」", "group:123", "m1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("mock 回复缺少 %q: %s", want, text)
		}
	}

	// 3) 未落库的 user 触发 → 纪律告警
	unseen := ev
	unseen.ID = "m2"
	envUnseen := env
	envUnseen.Event = unseen
	if got := chatText(t, client, mock.url, envUnseen); !strings.Contains(got, "未先落库") {
		t.Fatalf("未落库触发应报告违纪: %s", got)
	}

	// 4) 主动消息无需落库
	envProactive := env
	envProactive.Origin = envelope.OriginProactive
	envProactive.Event.ID = "m3"
	if got := chatText(t, client, mock.url, envProactive); !strings.Contains(got, "无需落库") {
		t.Fatalf("主动消息不应要求先落库: %s", got)
	}
}

func postBatch(client *http.Client, base string, br envelope.BatchRequest) error {
	body, _ := json.Marshal(br)
	resp, err := client.Post(base+"/v1/conversation/events", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

func chatText(t *testing.T, client *http.Client, base string, env envelope.TriggerEnvelope) string {
	t.Helper()
	block, err := env.Block()
	if err != nil {
		t.Fatal(err)
	}
	parts := []envelope.Part{envelope.TextPart(block + "\n" + env.Event.Text())}
	payload, _ := json.Marshal(map[string]any{
		"model": "m", "stream": false,
		"messages": []map[string]any{{"role": "user", "content": parts}},
	})
	resp, err := client.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("chat 请求: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &chat); err != nil || len(chat.Choices) == 0 {
		t.Fatalf("mock 回复不可解析: %s (%v)", body, err)
	}
	return chat.Choices[0].Message.Content
}
