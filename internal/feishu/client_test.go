package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseReceiver(t *testing.T) {
	cases := []struct {
		in       string
		wantType string
		wantID   string
		wantErr  bool
	}{
		{"user:ou_abc", "open_id", "ou_abc", false},
		{"open_id:ou_abc", "open_id", "ou_abc", false},
		{"chat:oc_x", "chat_id", "oc_x", false},
		{" email : a@b.c ", "email", "a@b.c", false},
		{"ou_abc", "", "", true},
		{"user:", "", "", true},
		{"phone:123", "", "", true},
	}
	for _, c := range cases {
		r, err := ParseReceiver(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("%q: err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && (r.IDType != c.wantType || r.ID != c.wantID) {
			t.Fatalf("%q: got %+v", c.in, r)
		}
	}
}

// fakeFeishu 模拟两个接口。sendReply 决定每次发送的 (HTTP 状态, 响应体)。
type fakeFeishu struct {
	tokenCalls atomic.Int32
	sendCalls  atomic.Int32
	sendReply  func(n int32, auth string) (int, any)
	lastBody   map[string]string
}

func (f *fakeFeishu) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		n := f.tokenCalls.Add(1)
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["app_id"] != "cli_x" || in["app_secret"] != "sec" {
			t.Errorf("token 请求参数不对: %v", in)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "t" + string(rune('0'+n)), "expire": 7200})
	})
	mux.HandleFunc("POST /open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		n := f.sendCalls.Add(1)
		if got := r.URL.Query().Get("receive_id_type"); got != "open_id" {
			t.Errorf("receive_id_type=%q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		status, body := f.sendReply(n, r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	return mux
}

func newClient(url string, now func() time.Time) *Client {
	return &Client{BaseURL: url, AppID: "cli_x", AppSecret: "sec", To: Receiver{"open_id", "ou_me"}, Now: now}
}

func ok(id string) any { return map[string]any{"code": 0, "data": map[string]any{"message_id": id}} }

func TestSendTextCachesToken(t *testing.T) {
	f := &fakeFeishu{sendReply: func(n int32, _ string) (int, any) { return 200, ok("om_1") }}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := newClient(srv.URL, nil)

	for i := 0; i < 3; i++ {
		id, err := c.SendText(context.Background(), "hi")
		if err != nil || id != "om_1" {
			t.Fatalf("send %d: id=%q err=%v", i, id, err)
		}
	}
	if f.tokenCalls.Load() != 1 {
		t.Fatalf("token 应只换 1 次，实际 %d", f.tokenCalls.Load())
	}
	var content map[string]string
	_ = json.Unmarshal([]byte(f.lastBody["content"]), &content)
	if f.lastBody["receive_id"] != "ou_me" || f.lastBody["msg_type"] != "text" || content["text"] != "hi" {
		t.Fatalf("消息体不对: %v", f.lastBody)
	}
}

func TestSendTextRefreshesExpiredToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &fakeFeishu{sendReply: func(n int32, _ string) (int, any) { return 200, ok("om") }}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := newClient(srv.URL, func() time.Time { return now })

	_, _ = c.SendText(context.Background(), "a")
	now = now.Add(7200*time.Second - 4*time.Minute) // 已进入提前 5 分钟的换新窗口
	_, _ = c.SendText(context.Background(), "b")
	if f.tokenCalls.Load() != 2 {
		t.Fatalf("过期前 5 分钟内应重新换 token，实际换了 %d 次", f.tokenCalls.Load())
	}
}

// 关键回归：失效码配非 200 状态时，也要清缓存重试（quantlab 版只看状态会漏掉）。
func TestSendTextRetriesInvalidTokenEvenWithNon200(t *testing.T) {
	f := &fakeFeishu{sendReply: func(n int32, auth string) (int, any) {
		if n == 1 {
			return 400, map[string]any{"code": 99991663, "msg": "invalid access token"}
		}
		if auth != "Bearer t2" {
			t.Errorf("重试应使用新 token，实际 %q", auth)
		}
		return 200, ok("om_retry")
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	c := newClient(srv.URL, nil)

	id, err := c.SendText(context.Background(), "x")
	if err != nil || id != "om_retry" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if f.tokenCalls.Load() != 2 || f.sendCalls.Load() != 2 {
		t.Fatalf("token=%d send=%d，应各 2 次", f.tokenCalls.Load(), f.sendCalls.Load())
	}
}

func TestSendTextInvalidTokenRetriesOnlyOnce(t *testing.T) {
	f := &fakeFeishu{sendReply: func(int32, string) (int, any) {
		return 200, map[string]any{"code": 99991663, "msg": "invalid access token"}
	}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	_, err := newClient(srv.URL, nil).SendText(context.Background(), "x")
	if err == nil || f.sendCalls.Load() != 2 {
		t.Fatalf("err=%v send=%d，应失败且只重试一次", err, f.sendCalls.Load())
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   any
		temp   bool
	}{
		{"5xx", 502, map[string]any{"code": 0}, true},
		{"429", 429, map[string]any{"code": 0}, true},
		{"feishu rate limit", 200, map[string]any{"code": 99991400, "msg": "frequency limit"}, true},
		{"bad receiver", 400, map[string]any{"code": 230001, "msg": "invalid receive_id"}, false},
		{"no permission", 200, map[string]any{"code": 230013, "msg": "bot has no availability"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeFeishu{sendReply: func(int32, string) (int, any) { return tc.status, tc.body }}
			srv := httptest.NewServer(f.handler(t))
			defer srv.Close()
			_, err := newClient(srv.URL, nil).SendText(context.Background(), "x")
			if err == nil {
				t.Fatal("应当失败")
			}
			if IsTemporary(err) != tc.temp {
				t.Fatalf("IsTemporary=%v want %v (%v)", IsTemporary(err), tc.temp, err)
			}
		})
	}
}

func TestNetworkErrorIsTemporary(t *testing.T) {
	c := newClient("http://127.0.0.1:1", nil)
	_, err := c.SendText(context.Background(), "x")
	if err == nil || !IsTemporary(err) {
		t.Fatalf("连不上应算临时错误: %v", err)
	}
}
