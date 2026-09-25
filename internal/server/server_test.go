package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var errTemp = errors.New("feishu down")
var errPerm = errors.New("bad receiver")

type fakeSender struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (f *fakeSender) SendText(_ context.Context, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.sent = append(f.sent, text)
	return "om_1", nil
}

func (f *fakeSender) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

type fakeQueue struct {
	mu    sync.Mutex
	items []string
}

func (q *fakeQueue) Add(caller, text, _ string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, caller+"|"+text)
	return "q1", nil
}

func (q *fakeQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func newTestServer(sender *fakeSender, q *fakeQueue, lim *Limiter) http.Handler {
	s := &Server{
		Tokens:    map[string]string{"tok-acme-0123456789": "acme", "tok-openwrt-0123456": "openwrt"},
		Sender:    sender,
		Queue:     q,
		Temporary: func(err error) bool { return errors.Is(err, errTemp) },
		Limiter:   lim,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s.Handler()
}

func do(t *testing.T, h http.Handler, method, path, ctype, auth, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

// acme.sh 的 notify/feishu.sh 就是这么发、这么判断成功的。
func TestHookAcmeCompatible(t *testing.T) {
	snd := &fakeSender{}
	h := newTestServer(snd, &fakeQueue{}, nil)
	body := `{"msg_type": "text","content": {"text": "[acme]\nRenew success\nexample.com"}}`
	rec, out := do(t, h, "POST", "/hook/tok-acme-0123456789", "application/json;charset=utf-8", "", body)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, out)
	}
	if !strings.Contains(out, `StatusCode":0`) {
		t.Fatalf("acme.sh 用子串 StatusCode\":0 判定成功，响应里没有: %s", out)
	}
	var r struct{ Code int }
	_ = json.Unmarshal([]byte(out), &r)
	if r.Code != 0 {
		t.Fatalf("quantlab 看 code，应为 0: %s", out)
	}
	if got := snd.last(); got != "[acme]\n[acme]\nRenew success\nexample.com" {
		t.Fatalf("消息文本不对: %q", got)
	}
}

func TestHookPostFlatten(t *testing.T) {
	snd := &fakeSender{}
	h := newTestServer(snd, &fakeQueue{}, nil)
	body := `{"msg_type":"post","content":{"post":{"zh_cn":{"title":"日报","content":[
		[{"tag":"text","text":"收益 "},{"tag":"a","text":"详情","href":"https://x/y"}],
		[{"tag":"text","text":"第二行"}]]}}}}`
	rec, out := do(t, h, "POST", "/hook/tok-acme-0123456789", "application/json", "", body)
	if rec.Code != 200 {
		t.Fatalf("status=%d %s", rec.Code, out)
	}
	if want := "[acme]\n日报\n收益 详情 (https://x/y)\n第二行"; snd.last() != want {
		t.Fatalf("got %q want %q", snd.last(), want)
	}
}

func TestHookRejects(t *testing.T) {
	h := newTestServer(&fakeSender{}, &fakeQueue{}, nil)
	rec, out := do(t, h, "POST", "/hook/nope", "application/json", "", `{"msg_type":"text","content":{"text":"x"}}`)
	if rec.Code != 401 || !strings.Contains(out, `"code":19001`) {
		t.Fatalf("坏 token: %d %s", rec.Code, out)
	}
	rec, _ = do(t, h, "POST", "/hook/tok-acme-0123456789", "application/json", "", `{"msg_type":"image","content":{}}`)
	if rec.Code != 400 {
		t.Fatalf("不支持的类型应 400，实际 %d", rec.Code)
	}
	rec, _ = do(t, h, "POST", "/hook/tok-acme-0123456789", "application/json", "", `not json`)
	if rec.Code != 400 {
		t.Fatalf("坏 JSON 应 400，实际 %d", rec.Code)
	}
}

func TestSendPlainAndJSON(t *testing.T) {
	snd := &fakeSender{}
	h := newTestServer(snd, &fakeQueue{}, nil)

	rec, out := do(t, h, "POST", "/send", "", "Bearer tok-openwrt-0123456", "EasyTier 出现 1 个异常节点\n未知节点 10.0.0.77")
	if rec.Code != 200 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("纯文本: %d %s", rec.Code, out)
	}
	if want := "[openwrt]\nEasyTier 出现 1 个异常节点\n未知节点 10.0.0.77"; snd.last() != want {
		t.Fatalf("got %q", snd.last())
	}

	rec, _ = do(t, h, "POST", "/send", "application/json", "Bearer tok-openwrt-0123456", `{"title":"标题","text":"正文"}`)
	if rec.Code != 200 || snd.last() != "[openwrt] 标题\n正文" {
		t.Fatalf("JSON: %d %q", rec.Code, snd.last())
	}
}

func TestSendAuth(t *testing.T) {
	h := newTestServer(&fakeSender{}, &fakeQueue{}, nil)
	for _, auth := range []string{"", "Bearer wrong", "tok-openwrt-0123456"} {
		rec, _ := do(t, h, "POST", "/send", "", auth, "x")
		if rec.Code != 401 {
			t.Fatalf("auth %q 应 401，实际 %d", auth, rec.Code)
		}
	}
	rec, _ := do(t, h, "POST", "/send", "", "Bearer tok-openwrt-0123456", "   ")
	if rec.Code != 400 {
		t.Fatalf("空消息应 400，实际 %d", rec.Code)
	}
}

func TestTemporaryFailureQueues(t *testing.T) {
	q := &fakeQueue{}
	h := newTestServer(&fakeSender{err: errTemp}, q, nil)
	rec, out := do(t, h, "POST", "/hook/tok-acme-0123456789", "application/json", "", `{"msg_type":"text","content":{"text":"x"}}`)
	if rec.Code != 200 || !strings.Contains(out, `"queued":true`) || !strings.Contains(out, `StatusCode":0`) {
		t.Fatalf("临时失败应入队并返回成功: %d %s", rec.Code, out)
	}
	if q.Len() != 1 {
		t.Fatalf("队列长度 %d", q.Len())
	}
}

func TestPermanentFailureReturnsError(t *testing.T) {
	q := &fakeQueue{}
	h := newTestServer(&fakeSender{err: errPerm}, q, nil)
	rec, out := do(t, h, "POST", "/send", "", "Bearer tok-openwrt-0123456", "x")
	if rec.Code != 502 || q.Len() != 0 {
		t.Fatalf("永久失败应 502 且不入队: %d %s len=%d", rec.Code, out, q.Len())
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	lim := &Limiter{Burst: 3, Window: 10 * time.Minute, Now: func() time.Time { return now }}
	snd := &fakeSender{}
	h := newTestServer(snd, &fakeQueue{}, lim)
	send := func() int {
		rec, _ := do(t, h, "POST", "/send", "", "Bearer tok-openwrt-0123456", "x")
		return rec.Code
	}
	for i := 0; i < 3; i++ {
		if c := send(); c != 200 {
			t.Fatalf("第 %d 条应放行，实际 %d", i+1, c)
		}
	}
	if c := send(); c != 429 {
		t.Fatalf("超限应 429，实际 %d", c)
	}
	// 另一个调用方不受影响
	rec, _ := do(t, h, "POST", "/hook/tok-acme-0123456789", "application/json", "", `{"msg_type":"text","content":{"text":"y"}}`)
	if rec.Code != 200 {
		t.Fatalf("acme 不应被 openwrt 的限流影响: %d", rec.Code)
	}
	now = now.Add(10 * time.Minute / 3) // 补回 1 个令牌
	if c := send(); c != 200 {
		t.Fatalf("补充令牌后应放行，实际 %d", c)
	}
}

func TestLimiterNotifiesOncePerWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	lim := &Limiter{Burst: 1, Window: time.Minute, Now: func() time.Time { return now }}
	lim.Allow("a")
	if _, n := lim.Allow("a"); !n {
		t.Fatal("第一次被限流应提醒")
	}
	if _, n := lim.Allow("a"); n {
		t.Fatal("同一窗口内不应重复提醒")
	}
}

func TestParseRate(t *testing.T) {
	if b, w, err := ParseRate("30/10m"); err != nil || b != 30 || w != 10*time.Minute {
		t.Fatalf("%d %v %v", b, w, err)
	}
	for _, bad := range []string{"30", "0/1m", "x/1m", "5/abc", "5/0s"} {
		if _, _, err := ParseRate(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
}

func TestFormatTruncatesOnRuneBoundary(t *testing.T) {
	out := format("c", "", strings.Repeat("中", maxText))
	if len(out) > maxText+64 || !strings.HasSuffix(out, "（已截断）") {
		t.Fatalf("len=%d suffix=%q", len(out), out[len(out)-20:])
	}
	if !json.Valid([]byte(`"` + strings.ReplaceAll(out, "\n", `\n`) + `"`)) {
		t.Fatal("截断切坏了 UTF-8")
	}
}

func TestHealthz(t *testing.T) {
	q := &fakeQueue{}
	_, _ = q.Add("a", "b", "")
	rec, out := do(t, newTestServer(&fakeSender{}, q, nil), "GET", "/healthz", "", "", "")
	if rec.Code != 200 || !strings.Contains(out, `"queued":1`) {
		t.Fatalf("%d %s", rec.Code, out)
	}
}
