package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noir017/pushme/internal/command"
	"github.com/noir017/pushme/internal/inbox"
)

func newCommandServer(t *testing.T) http.Handler {
	t.Helper()
	box, err := inbox.Open(filepath.Join(t.TempDir(), "inbox.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Tokens:    map[string]string{"tok-acme-0123456789": "acme", "tok-openwrt-0123456": "openwrt"},
		Receivers: map[string]string{"tok-phone-0123456789": "phone"},
		Sender:    &fakeSender{},
		Queue:     &fakeQueue{},
		Inbox:     box,
		Commands:  command.New(nil),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s.Handler()
}

func cmdJSON(to, id string, life int64) string {
	now := time.Now().Unix()
	b, _ := json.Marshal(map[string]any{"to": to, "id": id, "issued": now, "expires": now + life, "body": "grant unraid 1h", "sig": "c2lnbmF0dXJl"})
	return string(b)
}

type cmdResp struct {
	OK       bool
	Commands []command.Command
	Cursor   int64
}

func getCmds(t *testing.T, h http.Handler, auth, query string) cmdResp {
	t.Helper()
	rec, out := do(t, h, "GET", "/commands"+query, "", auth, "")
	if rec.Code != 200 {
		t.Fatalf("GET /commands%s: %d %s", query, rec.Code, out)
	}
	var r cmdResp
	_ = json.Unmarshal([]byte(out), &r)
	return r
}

func TestCommandsDisabledWithoutBox(t *testing.T) {
	h := newInboxServer(t, &fakeSender{}, nil)
	if rec, _ := do(t, h, "GET", "/commands/targets", "", recvTok, ""); rec.Code != 404 {
		t.Fatalf("没开应 404，实际 %d", rec.Code)
	}
}

func TestCommandFlow(t *testing.T) {
	h := newCommandServer(t)
	// listen 第一次连上：只拿当前序号，并登记在线
	if r := getCmds(t, h, callerTok, "?label=uniagent"); r.Cursor != 0 || len(r.Commands) != 0 {
		t.Fatalf("%+v", r)
	}
	rec, out := do(t, h, "GET", "/commands/targets", "", recvTok, "")
	if rec.Code != 200 || !strings.Contains(out, `"name":"openwrt"`) || !strings.Contains(out, `"label":"uniagent"`) || !strings.Contains(out, `"now":`) {
		t.Fatalf("在线名单：%d %s", rec.Code, out)
	}
	// 长轮询：投给自己的才唤醒
	done := make(chan cmdResp)
	go func() { done <- getCmds(t, h, callerTok, "?after=0&wait=10") }()
	time.Sleep(50 * time.Millisecond)
	if rec, out := do(t, h, "POST", "/commands", "application/json", recvTok, cmdJSON("acme", "0123456789abcdef", 120)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	if rec, out := do(t, h, "POST", "/commands", "application/json", recvTok, cmdJSON("openwrt", "fedcba9876543210", 120)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	select {
	case r := <-done:
		if len(r.Commands) != 1 || r.Commands[0].ID != "fedcba9876543210" || r.Commands[0].By != "phone" || r.Cursor != 2 {
			t.Fatalf("%+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("自己的指令来了应立即返回")
	}
	if r := getCmds(t, h, "Bearer tok-acme-0123456789", "?after=0"); len(r.Commands) != 1 || r.Commands[0].To != "acme" {
		t.Fatalf("各取各的：%+v", r)
	}
}

func TestCommandAuthAndValidation(t *testing.T) {
	h := newCommandServer(t)
	for _, auth := range []string{"", callerTok} {
		if rec, _ := do(t, h, "POST", "/commands", "application/json", auth, cmdJSON("openwrt", "0123456789abcdef", 60)); rec.Code != 401 {
			t.Fatalf("投递只认接收方 token（%q），实际 %d", auth, rec.Code)
		}
		if rec, _ := do(t, h, "GET", "/commands/targets", "", auth, ""); rec.Code != 401 {
			t.Fatalf("在线名单只认接收方 token（%q），实际 %d", auth, rec.Code)
		}
	}
	if rec, _ := do(t, h, "GET", "/commands?after=0", "", recvTok, ""); rec.Code != 401 {
		t.Fatalf("取指令只认调用方 token，实际 %d", rec.Code)
	}
	if rec, _ := do(t, h, "POST", "/commands", "application/json", recvTok, cmdJSON("nobody", "0123456789abcdef", 60)); rec.Code != 404 {
		t.Fatalf("收件方不是调用方应 404，实际 %d", rec.Code)
	}
	for name, body := range map[string]string{
		"坏 JSON":      "nope",
		"有效期太长":       cmdJSON("openwrt", "0123456789abcdef", 3600),
		"签名不是 base64": strings.Replace(cmdJSON("openwrt", "0123456789abcdef", 60), "c2lnbmF0dXJl", "%%%", 1),
		"已过期":         `{"to":"openwrt","id":"0123456789abcdef","issued":1,"expires":2,"body":"x","sig":"c2ln"}`,
	} {
		if rec, out := do(t, h, "POST", "/commands", "application/json", recvTok, body); rec.Code != 400 {
			t.Errorf("%s 应 400，实际 %d %s", name, rec.Code, out)
		}
	}
}
