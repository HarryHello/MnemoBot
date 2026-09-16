// Package journal 实现 bot 侧本地日志 (JSONL):
// 事件先落盘再入队, 断线/崩溃后按事件 ID 幂等补投 (服务端指纹去重兜底).
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

// timeNow 可测试注入的时间源.
var timeNow = time.Now

// Entry 单条日志记录.
type Entry struct {
	TS       int64            `json:"ts"` // epoch 毫秒, 写入时间
	Kind     string           `json:"kind"`
	Space    envelope.Space   `json:"space,omitempty"`   // enqueue 时的空间坐标
	Events   []envelope.Event `json:"events,omitempty"`  // enqueue 携带的事件
	Acked    []string         `json:"acked,omitempty"`   // 服务端确认接受的事件 ID
	Rejected []string         `json:"rejected,omitempty"` // 服务端终态拒绝的事件 ID (不再重试)
}

const (
	KindEnqueue = "enqueue"
	KindAck     = "ack"

	currentName   = "journal.jsonl"
	maxRotated    = 16 // 历史轮转文件保留数 — 未确认事件可能存在于任何文件, 不可激进修剪
)

// Journal 追加式日志: journal.jsonl 为当前文件, 写满轮转为 journal.<nano>.jsonl.
type Journal struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	file     *os.File
	size     int64
}

// Open 打开 (或创建) journal; dir 需可写.
func Open(dir string, maxBytes int64) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建 journal 目录: %w", err)
	}
	path := filepath.Join(dir, currentName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开 journal 文件: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Journal{dir: dir, maxBytes: maxBytes, file: f, size: info.Size()}, nil
}

// Append 追加一条日志; 超过大小上限自动轮转.
func (j *Journal) Append(entry Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	entry.TS = timeNowMillis()
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("序列化 journal 记录: %w", err)
	}
	line = append(line, '\n')
	if j.size > 0 && j.size+int64(len(line)) > j.maxBytes {
		if err := j.rotateLocked(); err != nil {
			return fmt.Errorf("轮转 journal: %w", err)
		}
	}
	n, err := j.file.Write(line)
	j.size += int64(n)
	return err
}

// Close 关闭文件句柄.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

func (j *Journal) rotateLocked() error {
	if err := j.file.Close(); err != nil {
		return err
	}
	// 纳秒级时间戳 (定长 19 位, 字典序=时间序): 毫秒级时间戳会在同一毫秒内
	// 的连续轮转中产生同名文件, os.Rename 直接覆盖丢数据
	stamp := strconv.FormatInt(timeNow().UnixNano(), 10)
	if err := os.Rename(
		filepath.Join(j.dir, currentName),
		filepath.Join(j.dir, "journal."+stamp+".jsonl"),
	); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(j.dir, currentName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	j.file = f
	j.size = 0
	j.pruneLocked()
	return nil
}

func (j *Journal) pruneLocked() {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return
	}
	var rotated []string
	for _, e := range entries {
		name := e.Name()
		if name != currentName && filepath.Ext(name) == ".jsonl" {
			rotated = append(rotated, name)
		}
	}
	if len(rotated) <= maxRotated {
		return
	}
	sort.Strings(rotated) // nano 时间戳定长, 字典序即时间序
	for _, name := range rotated[:len(rotated)-maxRotated] {
		_ = os.Remove(filepath.Join(j.dir, name))
	}
}

// Pending 回放产物: 未获服务端确认的事件及其空间坐标.
type Pending struct {
	Space envelope.Space
	Event envelope.Event
}

// Replay 扫描全部日志文件, 返回 enqueue 过但从未 ack/reject 的事件 (按原始顺序).
//
// 崩溃可能在文件尾部留下残行 — 统一跳过解析失败的行; ack 之后重复出现的
// 同 ID enqueue 会重新入队, 服务端指纹去重保证重发安全.
func Replay(dir string) ([]Pending, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 journal 目录: %w", err)
	}
	var names []string
	for _, e := range files {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	order := make([]string, 0, 64)
	byID := make(map[string]Pending, 64)
	done := make(map[string]struct{}, 64)

	for _, name := range names {
		lines, err := readLines(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, line := range lines {
			var entry Entry
			if err := json.Unmarshal(line, &entry); err != nil {
				continue // 崩溃留下的残行, 跳过
			}
			switch entry.Kind {
			case KindEnqueue:
				for _, ev := range entry.Events {
					if _, seen := byID[ev.ID]; !seen {
						order = append(order, ev.ID)
					}
					byID[ev.ID] = Pending{Space: entry.Space, Event: ev}
					delete(done, ev.ID) // 重新入队覆盖旧状态
				}
			case KindAck:
				for _, id := range entry.Acked {
					acked(done, id)
				}
				for _, id := range entry.Rejected {
					acked(done, id)
				}
			}
		}
	}

	out := make([]Pending, 0, len(order))
	for _, id := range order {
		if _, done := done[id]; done {
			continue
		}
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func acked(done map[string]struct{}, id string) { done[id] = struct{}{} }

func readLines(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	// 尾部无换行的残行同样丢弃 (不完整写入)
	return lines, nil
}

func timeNowMillis() int64 { return timeNow().UnixMilli() }
