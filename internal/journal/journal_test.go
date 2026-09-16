package journal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

func testEvent(id string) envelope.Event {
	return envelope.Event{
		ID:      id,
		TS:      1737091282000,
		Kind:    envelope.KindMessage,
		Actor:   &envelope.Actor{ExternalKey: "10001"},
		Content: []envelope.Part{envelope.TextPart("消息内容 " + id)},
	}
}

func TestEnqueueAckReplay(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sp := envelope.Space{Type: "group", ID: "1"}
	if err := j.Append(Entry{Kind: KindEnqueue, Space: sp, Events: []envelope.Event{testEvent("a"), testEvent("b")}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Entry{Kind: KindEnqueue, Space: sp, Events: []envelope.Event{testEvent("c")}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Entry{Kind: KindAck, Acked: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	pending, err := Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("应剩 2 个未确认事件, got %d", len(pending))
	}
	if pending[0].Event.ID != "b" || pending[1].Event.ID != "c" {
		t.Fatalf("回放顺序不符: %s, %s", pending[0].Event.ID, pending[1].Event.ID)
	}
	if pending[0].Space != sp {
		t.Fatalf("回放丢失空间坐标: %+v", pending[0].Space)
	}
}

func TestReplaySkipsMalformedTail(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sp := envelope.Space{Type: "group", ID: "1"}
	if err := j.Append(Entry{Kind: KindEnqueue, Space: sp, Events: []envelope.Event{testEvent("a")}}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	// 追加残行 (崩溃场景): 一行完整 JSON 后跟残缺行与无换行尾巴
	f, _ := os.OpenFile(filepath.Join(dir, currentName), os.O_WRONLY|os.O_APPEND, 0o644)
	_, _ = f.WriteString(`{"kind":"enqueue","space":{"type":"group","id":"1"},"events":[{"id":"b","ts":1,"content":[{"type":"text","text":"x"}]}]}` + "\n")
	_, _ = f.WriteString(`{"kind":"enqueue"` + "\n")
	_, _ = f.WriteString(`{"kind":"enqueue"` + "\n") // 连续残行
	f.Close()

	pending, err := Replay(dir)
	if err != nil {
		t.Fatalf("残行不应导致回放失败: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("应回放 2 个事件, got %d", len(pending))
	}
}

func TestRotationAcrossReplay(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, 256) // 极小上限, 强制频繁轮转
	if err != nil {
		t.Fatal(err)
	}
	sp := envelope.Space{Type: "group", ID: "1"}
	const n = 6
	for i := 0; i < n; i++ {
		if err := j.Append(Entry{Kind: KindEnqueue, Space: sp, Events: []envelope.Event{testEvent("ev-" + string(rune('a'+i)))}}); err != nil {
			t.Fatal(err)
		}
	}
	j.Close()

	entries, _ := os.ReadDir(dir)
	rotated := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".jsonl" && e.Name() != currentName {
			rotated++
		}
	}
	if rotated == 0 {
		t.Fatal("未发生轮转")
	}

	pending, err := Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != n {
		t.Fatalf("轮转后回放应覆盖全部 %d 个事件, got %d", n, len(pending))
	}
}
