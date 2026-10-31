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

	"github.com/noir017/pushme/internal/approval"
	"github.com/noir017/pushme/internal/inbox"
)

const callerTok = "Bearer tok-openwrt-0123456"

func newApprovalServer(t *testing.T, sender *fakeSender, lim *Limiter) http.Handler {
	t.Helper()
	dir := t.TempDir()
	box, err := inbox.Open(filepath.Join(dir, "inbox.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := approval.Open(filepath.Join(dir, "approvals.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Tokens:    map[string]string{"tok-acme-0123456789": "acme", "tok-openwrt-0123456": "openwrt"},
		Receivers: map[string]string{"tok-phone-0123456789": "phone"},
		Sender:    sender,
		Queue:     &fakeQueue{},
		Inbox:     box,
		Approvals: store,
		Limiter:   lim,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return s.Handler()
}

type created struct {
	OK        bool
	ID        string
	Challenge string
	Expires   int64
}

func createOK(t *testing.T, h http.Handler, body string) created {
	t.Helper()
	rec, out := do(t, h, "POST", "/approvals", "application/json", callerTok, body)
	if rec.Code != 200 {
		t.Fatalf("POST /approvals: %d %s", rec.Code, out)
	}
	var c created
	if err := json.Unmarshal([]byte(out), &c); err != nil || !c.OK || c.ID == "" || c.Challenge == "" || c.Expires == 0 {
		t.Fatalf("%s %v", out, err)
	}
	return c
}

type approvalResultResp struct {
	OK    bool
	State string
	Sig   string
	By    string
}

func getResult(t *testing.T, h http.Handler, id, query string) (int, approvalResultResp) {
	t.Helper()
	rec, out := do(t, h, "GET", "/approvals/"+id+query, "", callerTok, "")
	var r approvalResultResp
	_ = json.Unmarshal([]byte(out), &r)
	return rec.Code, r
}

func TestApprovalsDisabledWithoutStore(t *testing.T) {
	h := newInboxServer(t, &fakeSender{}, nil) // 有收件箱、没审批
	if rec, _ := do(t, h, "POST", "/approvals", "application/json", callerTok, `{"text":"x"}`); rec.Code != 404 {
		t.Fatalf("没开审批应 404，实际 %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/approvals", "", recvTok, ""); rec.Code != 404 {
		t.Fatalf("没开审批应 404，实际 %d", rec.Code)
	}
}

func TestApprovalFlow(t *testing.T) {
	snd := &fakeSender{}
	h := newApprovalServer(t, snd, nil)
	c := createOK(t, h, `{"title":"uniagent 申请提权","text":"unraid（root）1 小时\n理由：重启 newapi","ttl":120}`)

	// 收件箱里有一条带审批引用的消息
	r := getInbox(t, h, "?after=0")
	if len(r.Messages) != 1 || r.Messages[0].Approval == nil || r.Messages[0].Approval.ID != c.ID ||
		r.Messages[0].Approval.Expires != c.Expires || r.Messages[0].Text != "unraid（root）1 小时\n理由：重启 newapi" {
		t.Fatalf("%+v", r.Messages)
	}

	// 手机取待审批：原样的标题正文和 challenge
	rec, out := do(t, h, "GET", "/approvals", "", recvTok, "")
	var list struct {
		Approvals []approval.Approval
		Now       int64
	}
	_ = json.Unmarshal([]byte(out), &list)
	if rec.Code != 200 || len(list.Approvals) != 1 || list.Now == 0 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	if a := list.Approvals[0]; a.ID != c.ID || a.Challenge != c.Challenge || a.Caller != "openwrt" || a.Title != "uniagent 申请提权" {
		t.Fatalf("%+v", a)
	}

	if code, res := getResult(t, h, c.ID, ""); code != 200 || res.State != "pending" {
		t.Fatalf("%d %+v", code, res)
	}

	// 发起方长轮询，手机批准后立即返回
	done := make(chan approvalResultResp)
	go func() { _, res := getResult(t, h, c.ID, "?wait=10"); done <- res }()
	time.Sleep(50 * time.Millisecond)
	if rec, out := do(t, h, "POST", "/approvals/"+c.ID, "application/json", recvTok, `{"decision":"approve","sig":"c2lnbmF0dXJl"}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, out)
	}
	select {
	case res := <-done:
		if res.State != "approved" || res.Sig != "c2lnbmF0dXJl" || res.By != "phone" {
			t.Fatalf("%+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("有答复应立即返回")
	}

	// 只认第一次
	rec, out = do(t, h, "POST", "/approvals/"+c.ID, "application/json", recvTok, `{"decision":"deny"}`)
	if rec.Code != 409 || !strings.Contains(out, `"state":"approved"`) {
		t.Fatalf("第二次答复应 409：%d %s", rec.Code, out)
	}
	if rec, out := do(t, h, "GET", "/approvals", "", recvTok, ""); !strings.Contains(out, `"approvals":[]`) {
		t.Fatalf("答复后不该再列出：%d %s", rec.Code, out)
	}
}

func TestApprovalFeishuReminder(t *testing.T) {
	snd := &fakeSender{}
	h := newApprovalServer(t, snd, nil)
	createOK(t, h, `{"title":"申请","text":"正文"}`)
	deadline := time.Now().Add(2 * time.Second)
	for snd.last() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if want := "[openwrt] 待审批：申请\n正文\n\n请在手机 pushme App 上处理，5 分钟内有效。"; snd.last() != want {
		t.Fatalf("飞书提醒：%q", snd.last())
	}
}

func TestApprovalAuth(t *testing.T) {
	h := newApprovalServer(t, &fakeSender{}, nil)
	for _, auth := range []string{"", "Bearer wrong", recvTok} {
		if rec, _ := do(t, h, "POST", "/approvals", "application/json", auth, `{"text":"x"}`); rec.Code != 401 {
			t.Fatalf("发起只认调用方 token（%q），实际 %d", auth, rec.Code)
		}
	}
	c := createOK(t, h, `{"text":"x"}`)
	for _, auth := range []string{"", callerTok} {
		if rec, _ := do(t, h, "GET", "/approvals", "", auth, ""); rec.Code != 401 {
			t.Fatalf("列待审批只认接收方 token（%q），实际 %d", auth, rec.Code)
		}
		if rec, _ := do(t, h, "POST", "/approvals/"+c.ID, "application/json", auth, `{"decision":"deny"}`); rec.Code != 401 {
			t.Fatalf("答复只认接收方 token（%q），实际 %d", auth, rec.Code)
		}
	}
	if rec, _ := do(t, h, "GET", "/approvals/"+c.ID, "", recvTok, ""); rec.Code != 401 {
		t.Fatalf("取结果只认调用方 token，实际 %d", rec.Code)
	}
	if rec, _ := do(t, h, "GET", "/approvals/"+c.ID, "", "Bearer tok-acme-0123456789", ""); rec.Code != 404 {
		t.Fatalf("别的调用方取不到（404），实际 %d", rec.Code)
	}
}

func TestApprovalCreateValidation(t *testing.T) {
	h := newApprovalServer(t, &fakeSender{}, nil)
	for name, body := range map[string]string{
		"坏 JSON": `nope`,
		"空内容":    `{"title":"  ","text":""}`,
		"标题太长":   `{"title":"` + strings.Repeat("a", maxApprovalTitle+1) + `","text":"x"}`,
		"正文太长":   `{"text":"` + strings.Repeat("a", maxApprovalText+1) + `"}`,
		"负的 ttl": `{"text":"x","ttl":-1}`,
	} {
		if rec, out := do(t, h, "POST", "/approvals", "application/json", callerTok, body); rec.Code != 400 {
			t.Errorf("%s 应 400，实际 %d %s", name, rec.Code, out)
		}
	}
}

func TestApprovalAnswerValidation(t *testing.T) {
	h := newApprovalServer(t, &fakeSender{}, nil)
	c := createOK(t, h, `{"text":"x"}`)
	for name, body := range map[string]string{
		"批准没签名":       `{"decision":"approve"}`,
		"签名不是 base64": `{"decision":"approve","sig":"%%%"}`,
		"不认识的决定":      `{"decision":"maybe"}`,
		"坏 JSON":      `nope`,
	} {
		if rec, out := do(t, h, "POST", "/approvals/"+c.ID, "application/json", recvTok, body); rec.Code != 400 {
			t.Errorf("%s 应 400，实际 %d %s", name, rec.Code, out)
		}
	}
	if rec, _ := do(t, h, "POST", "/approvals/nope", "application/json", recvTok, `{"decision":"deny"}`); rec.Code != 404 {
		t.Fatalf("不存在应 404，实际 %d", rec.Code)
	}
	if rec, _ := do(t, h, "POST", "/approvals/"+c.ID, "application/json", recvTok, `{"decision":"deny","sig":"c2ln"}`); rec.Code != 200 {
		t.Fatalf("拒绝不需要签名，实际 %d", rec.Code)
	}
	if _, res := getResult(t, h, c.ID, ""); res.State != "denied" || res.Sig != "" {
		t.Fatalf("拒绝不留签名：%+v", res)
	}
}

func TestApprovalLimits(t *testing.T) {
	h := newApprovalServer(t, &fakeSender{}, nil)
	for i := 0; i < approval.MaxPending; i++ {
		createOK(t, h, `{"text":"x"}`)
	}
	if rec, _ := do(t, h, "POST", "/approvals", "application/json", callerTok, `{"text":"x"}`); rec.Code != 429 {
		t.Fatalf("同一调用方挂太多应 429，实际 %d", rec.Code)
	}

	now := time.Unix(1_700_000_000, 0)
	lim := &Limiter{Burst: 1, Window: 10 * time.Minute, Now: func() time.Time { return now }}
	h = newApprovalServer(t, &fakeSender{}, lim)
	do(t, h, "POST", "/send", "", callerTok, "先发一条消息")
	if rec, _ := do(t, h, "POST", "/approvals", "application/json", callerTok, `{"text":"x"}`); rec.Code != 429 {
		t.Fatalf("审批和消息共用限流，实际 %d", rec.Code)
	}
}
