package triage

import (
	"testing"
	"time"
)

func TestAtSelfTriggersAndCooldown(t *testing.T) {
	tr := New([]string{"小忆"}, 30*time.Second, time.Hour, true)
	now := time.Now()
	key := "group:1"

	if d := tr.Evaluate(key, Signal{SpaceType: "group", AtSelf: true}, now); !d.Trigger {
		t.Fatalf("@我应触发, got %+v", d)
	}
	if d := tr.Evaluate(key, Signal{SpaceType: "group", AtSelf: true}, now.Add(5*time.Second)); d.Trigger {
		t.Fatalf("冷却期内不应触发: %+v", d)
	}
	if d := tr.Evaluate(key, Signal{SpaceType: "group", AtSelf: true}, now.Add(31*time.Second)); !d.Trigger {
		t.Fatalf("冷却后应触发: %+v", d)
	}
}

func TestNicknameMention(t *testing.T) {
	tr := New([]string{"小忆"}, 30*time.Second, 0, true)
	now := time.Now()
	key := "group:1"

	if d := tr.Evaluate(key, Signal{SpaceType: "group", Text: "小夜和小忆都在吗"}, now); !d.Trigger {
		t.Fatalf("名字提及应触发, got %+v", d)
	}
	if d := tr.Evaluate(key, Signal{SpaceType: "group", Text: "普通聊天内容"}, now.Add(31*time.Second)); d.Trigger {
		t.Fatalf("普通消息不应触发: %+v", d)
	}
}

func TestReplyToSent(t *testing.T) {
	tr := New(nil, 30*time.Second, 0, true)
	now := time.Now()
	tr.MarkSent("m1")

	if d := tr.Evaluate("g:1", Signal{SpaceType: "group", ReplyTo: "m1"}, now); !d.Trigger {
		t.Fatalf("回复我的消息应触发, got %+v", d)
	}
	if d := tr.Evaluate("g:2", Signal{SpaceType: "group", ReplyTo: "unknown"}, now); d.Trigger {
		t.Fatalf("回复他人消息不应触发: %+v", d)
	}
}

func TestPrivateAlwaysAndDisabled(t *testing.T) {
	now := time.Now()
	tr := New(nil, 0, 0, true)
	if d := tr.Evaluate("p:1", Signal{SpaceType: "private"}, now); !d.Trigger {
		t.Fatalf("私聊应触发, got %+v", d)
	}
	trOff := New(nil, 0, 0, false)
	if d := trOff.Evaluate("p:1", Signal{SpaceType: "private"}, now); d.Trigger {
		t.Fatalf("私聊禁用后不应触发: %+v", d)
	}
}

func TestPokeSelf(t *testing.T) {
	tr := New(nil, 30*time.Second, 0, true)
	now := time.Now()
	if d := tr.Evaluate("g:1", Signal{SpaceType: "group", PokeSelf: true}, now); !d.Trigger {
		t.Fatalf("被戳应触发, got %+v", d)
	}
	if d := tr.Evaluate("g:1", Signal{SpaceType: "group", PokeSelf: true}, now.Add(10*time.Second)); d.Trigger {
		t.Fatalf("冷却期内被戳不应触发: %+v", d)
	}
}

func TestDueSpacesSilence(t *testing.T) {
	tr := New(nil, 0, time.Hour, true)
	now := time.Now()
	tr.MarkActivity("g:1", now.Add(-2*time.Hour))
	tr.MarkActivity("g:2", now)

	due := tr.DueSpaces(now)
	if len(due) != 1 || due[0] != "g:1" {
		t.Fatalf("DueSpaces 应只包含静默超时的空间, got %v", due)
	}
	if d := tr.DueSpaces(now); d != nil && len(d) != 1 {
		t.Fatalf("重复调用应稳定: %v", d)
	}
}

func TestSilenceDisabled(t *testing.T) {
	tr := New(nil, 0, 0, true)
	tr.MarkActivity("g:1", time.Now().Add(-24*time.Hour))
	if due := tr.DueSpaces(time.Now()); due != nil {
		t.Fatalf("silence=0 应禁用主动消息, got %v", due)
	}
}
