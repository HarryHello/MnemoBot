package forwarder

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
	"github.com/HarryHello/mnemo-bot/internal/journal"
)

var testSpace = envelope.Space{Type: "group", ID: "123456"}

func testEvent(id string) envelope.Event {
	return envelope.Event{
		ID:      id,
		TS:      1737091282000,
		Kind:    envelope.KindMessage,
		Actor:   &envelope.Actor{ExternalKey: "10001", DisplayName: "小明"},
		Content: []envelope.Part{envelope.TextPart("消息 " + id)},
	}
}

func newTestJournal(t *testing.T) (*journal.Journal, string) {
	t.Helper()
	dir := t.TempDir()
	jr, err := journal.Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return jr, dir
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFlushAllBatchesEvents(t *testing.T) {
	received := make(chan envelope.BatchRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/conversation/events" {
			http.NotFound(w, r)
			return
		}
		var br envelope.BatchRequest
		_ = json.NewDecoder(r.Body).Decode(&br)
		received <- br
		results := make([]envelope.EventResult, len(br.Events))
		for i := range results {
			results[i] = envelope.EventResult{Index: i, Status: "accepted"}
		}
		_ = json.NewEncoder(w).Encode(envelope.BatchResponse{Results: results})
	}))
	defer srv.Close()

	jr, dir := newTestJournal(t)
	log := quietLogger()
	fw := New(srv.URL, "key", "model", "onebot11", "qq", 5*time.Second, jr, log)
	go fw.Run()

	ev1 := envelope.Event{ID: "a", TS: 1, Content: []envelope.Part{envelope.TextPart("x")}}
	ev2 := envelope.Event{ID: "b", TS: 2, Content: []envelope.Part{envelope.TextPart("y")}}
	fw.Enqueue(testSpace, ev1)
	fw.Enqueue(testSpace, ev2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fw.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	select {
	case br := <-received:
		if len(br.Events) != 2 || br.Platform != "qq" || br.Space.ID != "123456" {
			t.Fatalf("批量请求不符: %+v", br)
		}
		if br.Events[0].ID != "a" || br.Events[1].ID != "b" {
			t.Fatalf("批量事件顺序不符: %s, %s", br.Events[0].ID, br.Events[1].ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("服务端未收到批量请求")
	}
	fw.Stop()

	pending, err := journal.Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("全部确认后 journal 应无 pending, got %d", len(pending))
	}
}

func TestRetainedEventsRequeueAfterFailure(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	jr, dir := newTestJournal(t)
	fw := New(srv.URL, "key", "model", "onebot11", "qq", 5*time.Second, jr, quietLogger())
	go fw.Run()

	fw.Enqueue(testSpace, testEvent("a"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fw.FlushAll(ctx); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	fw.Stop()

	if attempts < 2 {
		t.Fatalf("5xx 应触发重试, attempts=%d", attempts)
	}
	// 成功响应 results 为空 → 按全部成功 ack
	pending, err := journal.Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("重试成功后应无 pending, got %d", len(pending))
	}
}

func TestTriggerCarriesEnvelopeBlock(t *testing.T) {
	var chatBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			chatBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"你好呀"}}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	jr, _ := newTestJournal(t)
	fw := New(srv.URL, "key", "mnemosync", "onebot11", "qq", 5*time.Second, jr, quietLogger())

	env := envelope.TriggerEnvelope{
		Version: envelope.Version, Protocol: envelope.ProtocolOneBot11, Platform: "qq",
		Space: testSpace,
		Event: envelope.Event{
			ID: "e1", TS: 1,
			Content: []envelope.Part{envelope.TextPart("问候"), envelope.ImagePart("http://img/x.jpg")},
		},
		Origin: envelope.OriginUser,
	}
	reply, err := fw.Trigger(context.Background(), env)
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if reply != "你好呀" {
		t.Fatalf("回复不符: %q", reply)
	}
	// 线上 JSON 传输会对 < > 转义 (\u003c), 断言须基于解码后的 content
	var payload struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(chatBody, &payload); err != nil {
		t.Fatalf("解析触发请求体: %v", err)
	}
	var parts []envelope.Part
	if err := json.Unmarshal(payload.Messages[0].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) == 0 || !strings.Contains(parts[0].Text, envelope.EnvelopeBlockOpen) {
		t.Fatalf("触发请求未携带信封块: %s", chatBody)
	}
	if !strings.Contains(string(chatBody), "image_url") {
		t.Fatalf("触发请求未携带图片 part: %s", chatBody)
	}
}
