// OneBot v11 反向 WebSocket 接入与动作调用 (§5.1 传输模式).
//
// napcat 作为 WS 客户端连入; 事件下行与动作调用共用同一条连接,
// 动作请求-响应用 echo 字段配对; 鉴权走握手 access_token.
package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/HarryHello/mnemo-bot/internal/envelope"
)

// Handlers 是 adapter 的下游消费回调 (翻译完成后调用).
type Handlers interface {
	GroupMessage(ctx context.Context, tr *Translated)
	PrivateMessage(ctx context.Context, tr *Translated)
	Notice(ctx context.Context, ev *envelope.Event, space envelope.Space, pokeSelf bool)
}

// Messenger 是下行动作出口抽象 (Server 实现; Phase 3 传输变体同样实现).
type Messenger interface {
	SendGroupMsg(ctx context.Context, groupID int64, text string) (string, error)
	SendPrivateMsg(ctx context.Context, userID int64, text string) (string, error)
	GetMessage(ctx context.Context, messageID string) (json.RawMessage, error)
}

type actionResponse struct {
	Status  string          `json:"status"`
	RetCode int             `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Echo    string          `json:"echo"`
}

const callTimeout = 20 * time.Second

// Server 反向 WS 服务端: 接收 napcat 连入, 翻译事件, 提供动作调用.
type Server struct {
	listen   string
	token    string
	upgrader websocket.Upgrader
	tr       *Translator
	handler  Handlers
	log      *slog.Logger

	writeMu sync.Mutex
	nextID  atomic.Uint64

	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan actionResponse
}

func NewServer(listen, token string, tr *Translator, handler Handlers, log *slog.Logger) *Server {
	return &Server{
		listen:   listen,
		token:    token,
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		tr:       tr,
		handler:  handler,
		log:      log,
		pending:  make(map[string]chan actionResponse),
	}
}

// Run 阻塞运行 HTTP 监听, ctx 结束时优雅关闭.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWS)
	srv := &http.Server{Addr: s.listen, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("反向 WS 监听中 (等待 napcat 连入)", "addr", s.listen)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "access_token 无效", http.StatusUnauthorized)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	old := s.conn
	s.conn = conn
	s.mu.Unlock()
	if old != nil {
		s.log.Warn("新 napcat 连接顶替旧连接")
		_ = old.Close()
	}
	s.log.Info("napcat 已连入", "remote", r.RemoteAddr)

	defer func() {
		s.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.mu.Unlock()
		s.failPending()
		_ = conn.Close()
		s.log.Warn("napcat 连接断开")
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		s.dispatch(data)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	if auth := r.Header.Get("Authorization"); strings.TrimPrefix(auth, "Bearer ") == s.token && auth != "" {
		return true
	}
	return r.URL.Query().Get("access_token") == s.token
}

func (s *Server) dispatch(data []byte) {
	var probe struct {
		PostType string `json:"post_type"`
		Echo     string `json:"echo"`
		RetCode  *int   `json:"retcode"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		s.log.Warn("无法解析 WS 帧", "err", err)
		return
	}
	if probe.Echo != "" && probe.RetCode != nil {
		var resp actionResponse
		if err := json.Unmarshal(data, &resp); err == nil {
			s.resolve(probe.Echo, resp)
		}
		return
	}
	ctx := context.Background()
	switch probe.PostType {
	case "meta_event":
		// 心跳 / lifecycle — 忽略 (连接级事件已在 handleWS 处理)
	case "message", "message_sent":
		tr, err := s.tr.TranslateMessage(data)
		if err != nil {
			s.log.Warn("翻译 message 事件失败", "err", err)
			return
		}
		if tr == nil {
			return // 自回声
		}
		switch tr.Space.Type {
		case envelope.SpaceGroup:
			s.handler.GroupMessage(ctx, tr)
		default:
			s.handler.PrivateMessage(ctx, tr)
		}
	case "notice":
		ev, space, pokeSelf, ok := s.tr.TranslateNotice(data)
		if ok {
			s.handler.Notice(ctx, ev, space, pokeSelf)
		}
	case "request":
		// 好友/加群请求 — 一期不处理
	default:
		s.log.Warn("未知 post_type", "post_type", probe.PostType)
	}
}

// ── 动作调用 ──────────────────────────────────────────────

func (s *Server) call(ctx context.Context, action string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return nil, errors.New("napcat 未连接")
	}
	echo := fmt.Sprintf("%s-%d", action, s.nextID.Add(1))
	ch := make(chan actionResponse, 1)
	s.mu.Lock()
	s.pending[echo] = ch
	s.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	s.writeMu.Lock()
	err := conn.WriteJSON(map[string]any{"action": action, "params": params, "echo": echo})
	s.writeMu.Unlock()
	if err != nil {
		s.dropPending(echo)
		return nil, fmt.Errorf("发送动作 %s: %w", action, err)
	}

	select {
	case resp := <-ch:
		if resp.RetCode != 0 {
			return resp.Data, fmt.Errorf("动作 %s 失败: retcode=%d status=%s", action, resp.RetCode, resp.Status)
		}
		return resp.Data, nil
	case <-callCtx.Done():
		s.dropPending(echo)
		return nil, fmt.Errorf("动作 %s 超时: %w", action, callCtx.Err())
	}
}

func (s *Server) resolve(echo string, resp actionResponse) {
	s.mu.Lock()
	ch, ok := s.pending[echo]
	delete(s.pending, echo)
	s.mu.Unlock()
	if ok {
		ch <- resp
	}
}

func (s *Server) dropPending(echo string) {
	s.mu.Lock()
	delete(s.pending, echo)
	s.mu.Unlock()
}

func (s *Server) failPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for echo, ch := range s.pending {
		ch <- actionResponse{Status: "failed", RetCode: -1}
		delete(s.pending, echo)
	}
}

// SendGroupMsg 发送群文本消息, 返回发出的 message_id (回复我判定数据源).
func (s *Server) SendGroupMsg(ctx context.Context, groupID int64, text string) (string, error) {
	data, err := s.call(ctx, "send_group_msg", map[string]any{
		"group_id": groupID,
		"message":  []Segment{NewTextSegment(text)},
	})
	if err != nil {
		return "", err
	}
	return extractMessageID(data), nil
}

// SendPrivateMsg 发送私聊文本消息, 返回发出的 message_id.
func (s *Server) SendPrivateMsg(ctx context.Context, userID int64, text string) (string, error) {
	data, err := s.call(ctx, "send_private_msg", map[string]any{
		"user_id": userID,
		"message": []Segment{NewTextSegment(text)},
	})
	if err != nil {
		return "", err
	}
	return extractMessageID(data), nil
}

// GetMessage 拉取原始消息 (引用还原用).
func (s *Server) GetMessage(ctx context.Context, messageID string) (json.RawMessage, error) {
	var param any
	if n, err := strconv.ParseInt(messageID, 10, 64); err == nil {
		param = n
	} else {
		param = messageID
	}
	return s.call(ctx, "get_msg", map[string]any{"message_id": param})
}

func extractMessageID(data json.RawMessage) string {
	var d struct {
		MessageID any `json:"message_id"`
	}
	_ = json.Unmarshal(data, &d)
	switch v := d.MessageID.(type) {
	case float64:
		return fmtID(int64(v))
	case string:
		return v
	default:
		return ""
	}
}
