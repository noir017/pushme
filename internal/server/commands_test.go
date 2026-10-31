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

func TestCommandStatus(t *testing.T) {
	h := newCommandServer(t)
	for _, c := range []struct{ body, want string }{
		{`[1,2]`, "JSON object"},
		{`null`, "JSON object"},
		{`{"grants":`, "JSON object"},
		{`{"x":"` + strings.Repeat("a", command.MaxStatus) + `"}`, "at most"},
	} {
		if rec, out := do(t, h, "POST", "/commands/status", "application/json", callerTok, c.body); rec.Code != 400 || !strings.Contains(out, c.want) {
			t.Errorf("%.20s：应 400 %s，实际 %d %s", c.body, c.want, rec.Code, out)
		}
	}
	if rec, _ := do(t, h, "POST", "/commands/status", "application/json", recvTok, `{}`); rec.Code != 401 {
		t.Fatalf("接收方不能报状态，实际 %d", rec.Code)
	}
	getCmds(t, h, callerTok, "?label=uniagent")
	if rec, out := do(t, h, "POST", "/commands/status", "application/json", callerTok, "{\n  \"grants\": [{\"host\": \"unraid\", \"expires\": 1793430000}]\n}"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	_, out := do(t, h, "GET", "/commands/targets", "", recvTok, "")
	if !strings.Contains(out, `"label":"uniagent"`) || !strings.Contains(out, `"status":{"grants":[{"host":"unraid","expires":1793430000}]}`) || !strings.Contains(out, `"status_at":`) {
		t.Fatalf("在线名单应带上报的状态（压成一行）：%s", out)
	}
}

func TestGrantRequestFlow(t *testing.T) {
	h := newCommandServer(t)
	body := func(target, hosts string) string {
		b, _ := json.Marshal(map[string]string{"target": target, "hosts": hosts, "duration": "1h", "reason": "重启 newapi，要改配置"})
		return string(b)
	}
	if rec, _ := do(t, h, "POST", "/commands/requests", "application/json", recvTok, body("acme", "unraid")); rec.Code != 401 {
		t.Fatalf("接收方不能提申请：%d", rec.Code)
	}
	if rec, out := do(t, h, "POST", "/commands/requests", "application/json", callerTok, body("acme", "unraid;reboot")); rec.Code != 400 {
		t.Fatalf("机器名不合法应 400：%d %s", rec.Code, out)
	}
	if rec, _ := do(t, h, "POST", "/commands/requests", "application/json", callerTok, body("nobody", "unraid")); rec.Code != 404 {
		t.Fatalf("target 不是调用方应 404：%d", rec.Code)
	}
	rec, out := do(t, h, "POST", "/commands/requests", "application/json", callerTok, body("acme", "unraid"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	var created struct{ ID string }
	_ = json.Unmarshal([]byte(out), &created)

	// App 看得到，收件箱里有带 request 的提醒
	_, out = do(t, h, "GET", "/commands/requests", "", recvTok, "")
	if !strings.Contains(out, `"id":"`+created.ID+`"`) || !strings.Contains(out, `"caller":"openwrt"`) || !strings.Contains(out, `"target":"acme"`) || !strings.Contains(out, `"now":`) {
		t.Fatalf("待处理列表：%s", out)
	}
	_, out = do(t, h, "GET", "/inbox?after=0", "", recvTok, "")
	if !strings.Contains(out, `"request":{"id":"`+created.ID+`"`) || !strings.Contains(out, "openwrt 申请提权：unraid 1h") {
		t.Fatalf("收件箱：%s", out)
	}

	// 发申请的调用方长轮询结果；App 批准后立即返回
	done := make(chan string)
	go func() {
		_, out := do(t, h, "GET", "/commands/requests/"+created.ID+"?wait=10", "", callerTok, "")
		done <- out
	}()
	time.Sleep(50 * time.Millisecond)
	if rec, out := do(t, h, "POST", "/commands/requests/"+created.ID, "application/json", recvTok, `{"decision":"maybe"}`); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	if rec, out := do(t, h, "POST", "/commands/requests/"+created.ID, "application/json", recvTok, `{"decision":"approve"}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	select {
	case out := <-done:
		if !strings.Contains(out, `"state":"approved"`) {
			t.Fatalf("%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("批准后长轮询应立即返回")
	}
	if rec, _ := do(t, h, "POST", "/commands/requests/"+created.ID, "application/json", recvTok, `{"decision":"deny"}`); rec.Code != 409 {
		t.Fatalf("只认第一次，实际 %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/commands/requests/"+created.ID, "", "Bearer tok-acme-0123456789", ""); rec.Code != 404 {
		t.Fatalf("别的调用方取不到，实际 %d", rec.Code)
	}
	if _, out := do(t, h, "GET", "/commands/requests", "", recvTok, ""); strings.Contains(out, created.ID) {
		t.Fatalf("处理过的不再列出：%s", out)
	}
}
