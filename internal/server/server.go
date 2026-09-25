// Package server 提供 pushme 的 HTTP 接口：
//
//	POST /hook/{token}  飞书自定义机器人兼容入口（acme.sh 的 feishu 钩子、quantlab 的 webhook 通道零改动接入）
//	POST /send          原生入口：Authorization: Bearer <token>；body 为纯文本，或 JSON {"title","text"}
//	GET  /healthz
//
// 每个 token 对应一个调用方名，消息前自动加「[调用方]」；收件人固定，调用方不能指定。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxBody = 64 << 10
	// 飞书文本消息体上限约 150KB；个人告警远用不到，截断以免整条被拒。
	maxText = 20000
)

// Sender 真正把一条文本发出去（生产环境是飞书客户端）。
type Sender interface {
	SendText(ctx context.Context, text string) (messageID string, err error)
}

// Queue 接收暂时发不出去的消息（生产环境是 spool）。
type Queue interface {
	Add(caller, text, lastErr string) (string, error)
	Len() int
}

type Server struct {
	Tokens    map[string]string // token → 调用方名
	Sender    Sender
	Queue     Queue
	Temporary func(error) bool
	Limiter   *Limiter
	Log       *slog.Logger
	Timeout   time.Duration // 同步发送的等待上限，超时就入队
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /hook/{token}", s.hook)
	mux.HandleFunc("POST /send", s.send)
	return mux
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": s.Queue.Len()})
}

// ---- 飞书自定义机器人兼容入口 ----

// hookResp 的字段顺序即 JSON 输出顺序。acme.sh 的 notify/feishu.sh 用子串 `StatusCode":0`
// 判断成功（冒号后不能有空格），quantlab 看 code —— 两种都要满足。
type hookResp struct {
	Code          int            `json:"code"`
	Msg           string         `json:"msg"`
	StatusCode    int            `json:"StatusCode"`
	StatusMessage string         `json:"StatusMessage"`
	Data          map[string]any `json:"data"`
}

func hookReply(w http.ResponseWriter, status, code int, msg string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	writeJSON(w, status, hookResp{Code: code, Msg: msg, StatusCode: code, StatusMessage: msg, Data: data})
}

// 飞书自定义机器人的错误码，照抄以便调用方的既有处理逻辑能认出来。
const (
	codeBadToken   = 19001
	codeBadRequest = 19002
	codeFrequency  = 9499
	codeSendFailed = 19024
)

type hookBody struct {
	MsgType string `json:"msg_type"`
	Content struct {
		Text string `json:"text"`
		Post map[string]struct {
			Title   string `json:"title"`
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
				Href string `json:"href"`
			} `json:"content"`
		} `json:"post"`
	} `json:"content"`
}

// flatten 把 text / post 两种消息展平成纯文本。post 取第一个语言块（通常是 zh_cn）。
func (b hookBody) flatten() (string, bool) {
	switch b.MsgType {
	case "text":
		return b.Content.Text, b.Content.Text != ""
	case "post":
		for _, lang := range []string{"zh_cn", "en_us"} {
			if p, ok := b.Content.Post[lang]; ok {
				return flattenPost(p.Title, p.Content), true
			}
		}
		for _, p := range b.Content.Post {
			return flattenPost(p.Title, p.Content), true
		}
	}
	return "", false
}

func flattenPost(title string, lines [][]struct {
	Tag  string `json:"tag"`
	Text string `json:"text"`
	Href string `json:"href"`
}) string {
	var out []string
	if title != "" {
		out = append(out, title)
	}
	for _, line := range lines {
		var b strings.Builder
		for _, el := range line {
			b.WriteString(el.Text)
			if el.Tag == "a" && el.Href != "" && el.Href != el.Text {
				b.WriteString(" (" + el.Href + ")")
			}
		}
		out = append(out, b.String())
	}
	return strings.Join(out, "\n")
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.Tokens[r.PathValue("token")]
	if !ok {
		hookReply(w, http.StatusUnauthorized, codeBadToken, "incoming webhook access token invalid", nil)
		return
	}
	var body hookBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		hookReply(w, http.StatusBadRequest, codeBadRequest, "params error: "+err.Error(), nil)
		return
	}
	text, ok := body.flatten()
	if !ok || strings.TrimSpace(text) == "" {
		hookReply(w, http.StatusBadRequest, codeBadRequest, "params error: only msg_type text/post with non-empty content is supported", nil)
		return
	}
	res := s.deliver(r.Context(), caller, "", text)
	switch res.status {
	case http.StatusOK:
		hookReply(w, http.StatusOK, 0, "success", res.data())
	case http.StatusTooManyRequests:
		hookReply(w, res.status, codeFrequency, res.err, nil)
	default:
		hookReply(w, res.status, codeSendFailed, res.err, nil)
	}
}

// ---- 原生入口 ----

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	caller, ok := s.Tokens[strings.TrimSpace(tok)]
	if !found || !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "missing or invalid bearer token"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var title, text string
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		var in struct{ Title, Text string }
		if err := json.Unmarshal(raw, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
			return
		}
		title, text = in.Title, in.Text
	} else {
		text = string(raw)
	}
	if strings.TrimSpace(text) == "" && strings.TrimSpace(title) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "empty message"})
		return
	}
	res := s.deliver(r.Context(), caller, title, text)
	if res.status != http.StatusOK {
		writeJSON(w, res.status, map[string]any{"ok": false, "error": res.err})
		return
	}
	out := res.data()
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

// ---- 公共投递逻辑 ----

type result struct {
	status    int
	messageID string
	queued    bool
	err       string
}

func (r result) data() map[string]any {
	return map[string]any{"message_id": r.messageID, "queued": r.queued}
}

func format(caller, title, text string) string {
	var b strings.Builder
	b.WriteString("[" + caller + "]")
	if t := strings.TrimSpace(title); t != "" {
		b.WriteString(" " + t)
	}
	if t := strings.TrimRight(text, "\n"); strings.TrimSpace(t) != "" {
		b.WriteString("\n" + t)
	}
	out := b.String()
	if len(out) > maxText {
		cut := maxText
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "\n…（已截断）"
	}
	return out
}

func (s *Server) deliver(ctx context.Context, caller, title, text string) result {
	msg := format(caller, title, text)
	lg := s.log().With("caller", caller, "bytes", len(msg))

	if s.Limiter != nil {
		allowed, notify := s.Limiter.Allow(caller)
		if notify {
			// 限流通知本身不过限流器，每个调用方每个窗口最多一条。
			go s.sendDetached("pushme", format("pushme", "限流", "调用方 "+caller+" 在 "+
				s.Limiter.Window.String()+" 内超过 "+strconv.Itoa(s.Limiter.Burst)+" 条，后续消息被丢弃直到窗口恢复。"))
		}
		if !allowed {
			lg.Warn("rate limited")
			return result{status: http.StatusTooManyRequests, err: "rate limited"}
		}
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	id, err := s.Sender.SendText(sctx, msg)
	if err == nil {
		lg.Info("sent", "msg_id", id)
		return result{status: http.StatusOK, messageID: id}
	}
	if s.Temporary != nil && s.Temporary(err) {
		if _, qerr := s.Queue.Add(caller, msg, err.Error()); qerr != nil {
			lg.Error("send failed and enqueue failed", "err", err, "queue_err", qerr)
			return result{status: http.StatusBadGateway, err: "send failed: " + err.Error()}
		}
		lg.Warn("send failed, queued for retry", "err", err)
		return result{status: http.StatusOK, queued: true}
	}
	lg.Error("send failed (permanent)", "err", err)
	return result{status: http.StatusBadGateway, err: "send failed: " + err.Error()}
}

func (s *Server) sendDetached(caller, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Sender.SendText(ctx, msg); err != nil && s.Temporary != nil && s.Temporary(err) {
		_, _ = s.Queue.Add(caller, msg, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- 限流 ----

// Limiter 是每个调用方一个令牌桶：容量 Burst，每 Window 补满。
type Limiter struct {
	Burst  int
	Window time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens     float64
	last       time.Time
	notifiedAt time.Time
}

var ErrBadRate = errors.New("限流格式应为 <条数>/<时长>，例如 30/10m")

// ParseRate 解析 "30/10m"。
func ParseRate(s string) (int, time.Duration, error) {
	n, d, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, 0, ErrBadRate
	}
	burst, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || burst <= 0 {
		return 0, 0, ErrBadRate
	}
	win, err := time.ParseDuration(d)
	if err != nil || win <= 0 {
		return 0, 0, ErrBadRate
	}
	return burst, win, nil
}

// Allow 返回是否放行；notify 为 true 表示这次是本窗口内第一次被限流，应发一条提醒。
func (l *Limiter) Allow(caller string) (allowed, notify bool) {
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	b, ok := l.buckets[caller]
	if !ok {
		b = &bucket{tokens: float64(l.Burst), last: now}
		l.buckets[caller] = b
	}
	rate := float64(l.Burst) / l.Window.Seconds()
	b.tokens = min(float64(l.Burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, false
	}
	if now.Sub(b.notifiedAt) >= l.Window {
		b.notifiedAt = now
		return false, true
	}
	return false, false
}
