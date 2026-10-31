package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noir017/pushme/internal/approval"
	"github.com/noir017/pushme/internal/inbox"
	"github.com/noir017/pushme/internal/server"
)

const (
	askCallerTok = "tok-grant-0123456789"
	askPhoneTok  = "tok-phone-0123456789"
)

type nopSender struct{}

func (nopSender) SendText(context.Context, string) (string, error) { return "om_1", nil }

type nopQueue struct{}

func (nopQueue) Add(string, string, string) (string, error) { return "", nil }
func (nopQueue) Len() int                                   { return 0 }

func newPushme(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	box, _ := inbox.Open(filepath.Join(dir, "inbox.json"), nil)
	store, _ := approval.Open(filepath.Join(dir, "approvals.json"), nil)
	s := &server.Server{
		Tokens:    map[string]string{askCallerTok: "agent-grant"},
		Receivers: map[string]string{askPhoneTok: "phone"},
		Sender:    nopSender{},
		Queue:     nopQueue{},
		Inbox:     box,
		Approvals: store,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// phone 模拟手机 App：等出现待审批，按 mutate 改过的内容签名（或拒绝）后答复。
func phone(t *testing.T, base string, key *ecdsa.PrivateKey, approve bool, mutate func(*approval.Approval)) {
	t.Helper()
	call := func(method, path, body string) []byte {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+askPhoneTok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return nil
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return out
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var list struct{ Approvals []approval.Approval }
		_ = json.Unmarshal(call("GET", "/approvals", ""), &list)
		if len(list.Approvals) == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		a := list.Approvals[0]
		if !approve {
			call("POST", "/approvals/"+a.ID, `{"decision":"deny"}`)
			return
		}
		if mutate != nil {
			mutate(&a)
		}
		d := sha256.Sum256(approval.Payload(a))
		sig, _ := ecdsa.SignASN1(rand.Reader, key, d[:])
		call("POST", "/approvals/"+a.ID, `{"decision":"approve","sig":"`+base64.StdEncoding.EncodeToString(sig)+`"}`)
		return
	}
	t.Error("手机一直没等到待审批")
}

func genKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAskOutcomes(t *testing.T) {
	key := genKey(t)
	for _, tc := range []struct {
		name    string
		signer  *ecdsa.PrivateKey
		approve bool
		mutate  func(*approval.Approval)
		want    string
	}{
		{"批准", key, true, nil, resApproved},
		{"拒绝", key, false, nil, resDenied},
		{"别的密钥签的", genKey(t), true, nil, resBadSignature},
		{"手机看到的内容被改过", key, true, func(a *approval.Approval) { a.Text = "oracle（root）12 小时" }, resBadSignature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newPushme(t)
			go phone(t, ts.URL, tc.signer, tc.approve, tc.mutate)
			res, err := runAsk(context.Background(), askConfig{
				URL: ts.URL, Token: askCallerTok, Pub: &key.PublicKey,
				Title: "uniagent 申请提权", Text: "unraid（root）1 小时\n理由：重启 newapi", TTL: time.Minute,
			})
			if err != nil || res != tc.want {
				t.Fatalf("got %q %v, want %q", res, err, tc.want)
			}
		})
	}
}

func TestAskErrors(t *testing.T) {
	key := genKey(t)
	ts := newPushme(t)
	if _, err := runAsk(context.Background(), askConfig{URL: ts.URL, Token: "wrong-token-0123456", Pub: &key.PublicKey, Text: "x"}); err == nil {
		t.Fatal("token 不对应报错")
	}
	if _, err := runAsk(context.Background(), askConfig{URL: "http://127.0.0.1:1", Token: askCallerTok, Pub: &key.PublicKey, Text: "x"}); err == nil {
		t.Fatal("连不上应报错")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := runAsk(ctx, askConfig{URL: ts.URL, Token: askCallerTok, Pub: &key.PublicKey, Text: "没人答复"}); err == nil {
		t.Fatal("被中断应报错")
	}
}

func writePub(t *testing.T, k *ecdsa.PrivateKey) string {
	t.Helper()
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	path := filepath.Join(t.TempDir(), "approver.pub")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAskMainExitCodes(t *testing.T) {
	key := genKey(t)
	pub := writePub(t, key)
	ts := newPushme(t)
	env := func(k string) string {
		return map[string]string{"PUSHME_URL": ts.URL, "PUSHME_TOKEN": askCallerTok}[k]
	}

	go phone(t, ts.URL, key, true, nil)
	var out, errb bytes.Buffer
	if code := askMain([]string{"-pubkey", pub, "-title", "申请", "-text", "unraid 1h"}, env, &out, &errb); code != 0 || out.String() != "approved\n" {
		t.Fatalf("批准应 exit 0：%d %q %q", code, out.String(), errb.String())
	}

	go phone(t, ts.URL, key, false, nil)
	out.Reset()
	if code := askMain([]string{"-pubkey", pub, "-text", "x"}, env, &out, &errb); code != 1 || out.String() != "denied\n" {
		t.Fatalf("拒绝应 exit 1：%d %q", code, out.String())
	}

	for name, args := range map[string][]string{
		"缺公钥":     {"-text", "x"},
		"没内容":     {"-pubkey", pub},
		"公钥文件不在":  {"-pubkey", "/nonexistent", "-text", "x"},
		"坏 UTF-8": {"-pubkey", pub, "-text", "\xff"},
		"坏参数":     {"-nope"},
	} {
		if code := askMain(args, env, io.Discard, io.Discard); code != 2 {
			t.Errorf("%s 应 exit 2，实际 %d", name, code)
		}
	}
	if code := askMain([]string{"-pubkey", pub, "-text", "x"}, func(string) string { return "" }, io.Discard, io.Discard); code != 2 {
		t.Errorf("没有地址和 token 应 exit 2，实际 %d", code)
	}
}

func TestFingerprintMain(t *testing.T) {
	key := genKey(t)
	var out bytes.Buffer
	if code := fingerprintMain([]string{writePub(t, key)}, &out, io.Discard); code != 0 {
		t.Fatal(code)
	}
	want, _ := approval.Fingerprint(&key.PublicKey)
	if strings.TrimSpace(out.String()) != want {
		t.Fatalf("%q want %q", out.String(), want)
	}
	if code := fingerprintMain(nil, io.Discard, io.Discard); code != 2 {
		t.Fatal(code)
	}
}
