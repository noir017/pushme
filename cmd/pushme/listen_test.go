package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noir017/pushme/internal/command"
	"github.com/noir017/pushme/internal/inbox"
	"github.com/noir017/pushme/internal/server"
)

const listenTok = "tok-grant-0123456789"

func newCommandPushme(t *testing.T) (*httptest.Server, *inbox.Inbox) {
	t.Helper()
	box, _ := inbox.Open(filepath.Join(t.TempDir(), "inbox.json"), nil)
	s := &server.Server{
		Tokens:    map[string]string{listenTok: "agent-grant", "tok-other-0123456789": "hermes-grant"},
		Receivers: map[string]string{askPhoneTok: "phone"},
		Sender:    nopSender{},
		Queue:     nopQueue{},
		Inbox:     box,
		Commands:  command.New(nil),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, box
}

// sendCommand 模拟手机：按约定签名后投递。
func sendCommand(t *testing.T, base string, key *ecdsa.PrivateKey, c command.Command) {
	t.Helper()
	if c.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		c.ID = hex.EncodeToString(b)
	}
	d := sha256.Sum256(command.Payload(c))
	sig, _ := ecdsa.SignASN1(rand.Reader, key, d[:])
	c.Sig = base64.StdEncoding.EncodeToString(sig)
	body, _ := json.Marshal(c)
	req, _ := http.NewRequest("POST", base+"/commands", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+askPhoneTok)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("投递失败 %d", res.StatusCode)
	}
}

type runs struct {
	mu   sync.Mutex
	args [][]string
	env  [][]string
}

func (r *runs) run(_ context.Context, name string, args, env []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, append([]string{name}, args...))
	r.env = append(r.env, env)
	return "ok", nil
}

func (r *runs) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.args)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("一直没等到：" + what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestListenExecutesSignedCommands(t *testing.T) {
	key := genKey(t)
	ts, box := newCommandPushme(t)
	r := &runs{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = runListen(ctx, listenConfig{URL: ts.URL, Token: listenTok, To: "agent-grant", Label: "uniagent",
			Pub: &key.PublicKey, Exec: "/usr/local/sbin/agent-grant", ExecArgs: []string{"_cmd"}, Run: r.run})
	}()
	// listen 连上后才投（第一次只拿当前序号，不回放）
	waitFor(t, "listen 上线", func() bool {
		req, _ := http.NewRequest("GET", ts.URL+"/commands/targets", nil)
		req.Header.Set("Authorization", "Bearer "+askPhoneTok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return strings.Contains(string(b), `"label":"uniagent"`)
	})
	now := time.Now().Unix()
	ok := command.Command{ID: "aaaaaaaaaaaaaaaa", To: "agent-grant", Issued: now, Expires: now + 120, Body: "grant unraid,openwrt 1h"}
	sendCommand(t, ts.URL, key, ok)
	waitFor(t, "执行", func() bool { return r.count() == 1 })
	if got := strings.Join(r.args[0], " "); got != "/usr/local/sbin/agent-grant _cmd grant unraid,openwrt 1h" {
		t.Fatalf("参数：%q", got)
	}
	if env := strings.Join(r.env[0], " "); !strings.Contains(env, "PUSHME_CMD_ID=aaaaaaaaaaaaaaaa") || !strings.Contains(env, "PUSHME_CMD_BY=phone") {
		t.Fatalf("环境变量：%q", env)
	}

	// 下面这些都不该执行，并各推一条警告
	sendCommand(t, ts.URL, key, ok) // 同一 id 再来一遍
	sendCommand(t, ts.URL, genKey(t), command.Command{To: "agent-grant", Issued: now, Expires: now + 120, Body: "grant all 12h"})
	sendCommand(t, ts.URL, key, command.Command{To: "agent-grant", Issued: now + 3600, Expires: now + 3660, Body: "grant all 12h"})
	waitFor(t, "三条警告", func() bool {
		msgs, _, _ := box.After(0, 100)
		return len(msgs) >= 3
	})
	if r.count() != 1 {
		t.Fatalf("验不过的不该执行：%v", r.args)
	}
	msgs, _, _ := box.After(0, 100)
	var reasons []string
	for _, m := range msgs {
		reasons = append(reasons, m.Text)
	}
	all := strings.Join(reasons, "|")
	for _, want := range []string{"重放", "签名验不过", "签发时间在未来"} {
		if !strings.Contains(all, want) {
			t.Errorf("警告里应有「%s」：%s", want, all)
		}
	}
	if msgs[0].Caller != "agent-grant" || !strings.Contains(msgs[0].Title, "uniagent 拒绝了一条手机指令") {
		t.Errorf("警告经 /send 发出：%+v", msgs[0])
	}
}

func TestListenRejectsMisaddressedAndExpired(t *testing.T) {
	key := genKey(t)
	r := &runs{}
	cfg := listenConfig{To: "agent-grant", Label: "uniagent", Pub: &key.PublicKey, Exec: "x", Run: r.run,
		Client: &http.Client{Timeout: time.Second}, URL: "http://127.0.0.1:1", Now: time.Now}
	sign := func(c command.Command) command.Command {
		d := sha256.Sum256(command.Payload(c))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, d[:])
		c.Sig = base64.StdEncoding.EncodeToString(sig)
		return c
	}
	now := time.Now().Unix()
	logf := func(string, ...any) {}
	seen := map[string]int64{}
	handleCommand(context.Background(), cfg, sign(command.Command{ID: "1111111111111111", To: "hermes-grant", Issued: now, Expires: now + 60, Body: "grant all 1h"}), seen, logf)
	handleCommand(context.Background(), cfg, sign(command.Command{ID: "2222222222222222", To: "agent-grant", Issued: now - 200, Expires: now - 10, Body: "grant all 1h"}), seen, logf)
	handleCommand(context.Background(), cfg, sign(command.Command{ID: "3333333333333333", To: "agent-grant", Issued: now, Expires: now + 3600, Body: "grant all 1h"}), seen, logf)
	if r.count() != 0 {
		t.Fatalf("发给别人的、过期的、有效期太长的都不该执行：%v", r.args)
	}
	handleCommand(context.Background(), cfg, sign(command.Command{ID: "4444444444444444", To: "agent-grant", Issued: now, Expires: now + 60, Body: "revoke"}), seen, logf)
	if r.count() != 1 {
		t.Fatal("正常的应执行")
	}
}

func TestListenMainUsage(t *testing.T) {
	if code := listenMain([]string{"-to", "x"}, func(string) string { return "" }, io.Discard, io.Discard); code != 2 {
		t.Fatalf("缺参数应 exit 2，实际 %d", code)
	}
	ts, _ := newCommandPushme(t)
	env := func(k string) string {
		return map[string]string{"PUSHME_URL": ts.URL, "PUSHME_TOKEN": "wrong-token-0123456789"}[k]
	}
	pub := writePub(t, genKey(t))
	if code := listenMain([]string{"-to", "agent-grant", "-pubkey", pub, "-exec", "/bin/true"}, env, io.Discard, io.Discard); code != 1 {
		t.Fatalf("token 不对应直接退出（exit 1），实际 %d", code)
	}
}
