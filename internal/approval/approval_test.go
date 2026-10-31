package approval

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openAt(t *testing.T, path string, c *clock) *Store {
	t.Helper()
	s, err := Open(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateAnswerOnce(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	s := openAt(t, filepath.Join(t.TempDir(), "a.json"), c)
	a, err := s.Create("agent-grant", "申请提权", "unraid 1h", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != Pending || a.Expires != 1_700_000_000+300 || len(a.ID) != 16 || len(a.Challenge) != 32 {
		t.Fatalf("默认 5 分钟、id 16 位、challenge 32 位：%+v", a)
	}
	if p := s.Pending(); len(p) != 1 || p[0].ID != a.ID {
		t.Fatalf("%+v", p)
	}
	got, err := s.Answer(a.ID, "phone", true, "c2ln")
	if err != nil || got.State != Approved || got.Sig != "c2ln" || got.By != "phone" || got.Answered == 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.Answer(a.ID, "phone", false, ""); !errors.Is(err, ErrNotPending) {
		t.Fatalf("只认第一次答复：%v", err)
	}
	if len(s.Pending()) != 0 {
		t.Fatal("答复后不该还在 pending 里")
	}
	if _, err := s.Answer("nope", "phone", true, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestDenyDropsSig(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "a.json"), &clock{time.Unix(1_700_000_000, 0)})
	a, _ := s.Create("c", "t", "x", 0)
	got, _ := s.Answer(a.ID, "phone", false, "c2ln")
	if got.State != Denied || got.Sig != "" {
		t.Fatalf("%+v", got)
	}
}

func TestTTLClamped(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "a.json"), &clock{time.Unix(1_700_000_000, 0)})
	a, _ := s.Create("c", "t", "x", time.Second)
	b, _ := s.Create("c", "t", "x", 10*time.Hour)
	if a.Expires-a.Created != 30 || b.Expires-b.Created != 3600 {
		t.Fatalf("ttl 应截到 [30s, 1h]：%d %d", a.Expires-a.Created, b.Expires-b.Created)
	}
}

func TestExpireAndPrune(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	s := openAt(t, filepath.Join(t.TempDir(), "a.json"), c)
	a, _ := s.Create("c", "t", "x", time.Minute)
	c.t = c.t.Add(time.Minute)
	if got, ok := s.Get(a.ID); !ok || got.State != Expired {
		t.Fatalf("到点应为 expired：%+v", got)
	}
	if _, err := s.Answer(a.ID, "phone", true, "x"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("过期后不能再批：%v", err)
	}
	c.t = c.t.Add(keepDone)
	if _, ok := s.Get(a.ID); ok {
		t.Fatal("终态超过 keepDone 应被清掉")
	}
}

func TestMaxPendingPerCaller(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "a.json"), &clock{time.Unix(1_700_000_000, 0)})
	var first Approval
	for i := 0; i < MaxPending; i++ {
		a, err := s.Create("c", "t", "x", 0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = a
		}
	}
	if _, err := s.Create("c", "t", "x", 0); !errors.Is(err, ErrTooMany) {
		t.Fatalf("超过上限应 ErrTooMany：%v", err)
	}
	if _, err := s.Create("other", "t", "x", 0); err != nil {
		t.Fatalf("别的调用方不受影响：%v", err)
	}
	s.Answer(first.ID, "phone", false, "")
	if _, err := s.Create("c", "t", "x", 0); err != nil {
		t.Fatalf("答复一条后应能再发：%v", err)
	}
}

func TestPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	c := &clock{time.Unix(1_700_000_000, 0)}
	a, _ := openAt(t, path, c).Create("c", "标题", "正文\n第二行", 0)
	s := openAt(t, path, c)
	got, ok := s.Get(a.ID)
	if !ok || got != a {
		t.Fatalf("重启后应原样读回：%+v", got)
	}
	if _, err := s.Answer(a.ID, "phone", true, "c2ln"); err != nil {
		t.Fatal(err)
	}
	if got, _ := openAt(t, path, c).Get(a.ID); got.State != Approved {
		t.Fatalf("答复也要落盘：%+v", got)
	}
}

func TestWaitWakesOnAnswer(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "a.json"), &clock{time.Unix(1_700_000_000, 0)})
	a, _ := s.Create("c", "t", "x", 0)
	done := make(chan struct{})
	start := time.Now()
	go func() { s.Wait(context.Background(), a.ID, 10*time.Second); close(done) }()
	time.Sleep(50 * time.Millisecond)
	s.Answer(a.ID, "phone", false, "")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("有答复应立即醒来")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("醒得太慢")
	}
}

func TestWaitReturnsAtExpiry(t *testing.T) {
	// 真时钟：ttl 截到 30s 太长，这里直接改到期时刻
	s, _ := Open(filepath.Join(t.TempDir(), "a.json"), nil)
	a, _ := s.Create("c", "t", "x", 0)
	s.mu.Lock()
	s.items[a.ID].Expires = time.Now().Add(time.Second).Unix()
	s.mu.Unlock()
	start := time.Now()
	s.Wait(context.Background(), a.ID, 10*time.Second)
	if time.Since(start) > 3*time.Second {
		t.Fatal("到期应醒来")
	}
	if got, _ := s.Get(a.ID); got.State != Expired {
		t.Fatalf("%+v", got)
	}
}

// ---- 签名 ----

func TestPayloadFormat(t *testing.T) {
	a := Approval{ID: "0123456789abcdef", Challenge: "c", Expires: 1700000300, Title: "t", Text: "x"}
	want := "pushme-approval-v1\n0123456789abcdef\nc\napprove\n1700000300\n" +
		"e3b98a4da31a127d4bde6e43033f66ba274cab0eb7eb1c70ec41402bf6273dd8\n" +
		"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	if got := string(Payload(a)); got != want {
		t.Fatalf("签名内容是 App 与发起方共同的契约，改了两边都得改：\n%s", got)
	}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sign(t *testing.T, k *ecdsa.PrivateKey, a Approval) string {
	t.Helper()
	d := sha256.Sum256(Payload(a))
	sig, err := ecdsa.SignASN1(rand.Reader, k, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(sig)
}

func TestVerify(t *testing.T) {
	k := newKey(t)
	a := Approval{ID: "id", Challenge: "ch", Expires: 1, Title: "uniagent 申请提权", Text: "unraid 1h"}
	a.Sig = sign(t, k, a)
	if err := Verify(&k.PublicKey, a); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Approval){
		"改正文":         func(b *Approval) { b.Text = "oracle 12h" },
		"挪内容":         func(b *Approval) { b.Title, b.Text = "uniagent 申请提权\nunraid", " 1h" },
		"换 challenge": func(b *Approval) { b.Challenge = "other" },
		"改到期":         func(b *Approval) { b.Expires = 2 },
		"坏签名":         func(b *Approval) { b.Sig = "!!" },
		"空签名":         func(b *Approval) { b.Sig = "" },
	} {
		b := a
		mod(&b)
		if Verify(&k.PublicKey, b) == nil {
			t.Errorf("%s 后不该验过", name)
		}
	}
	if Verify(&newKey(t).PublicKey, a) == nil {
		t.Error("换了公钥不该验过")
	}
}

func pemOf(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestParsePublicKeyAndFingerprint(t *testing.T) {
	k := newKey(t)
	pub, err := ParsePublicKey(pemOf(t, &k.PublicKey))
	if err != nil || !pub.Equal(&k.PublicKey) {
		t.Fatalf("%v", err)
	}
	fp, err := Fingerprint(pub)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{4}( [0-9a-f]{4}){3}$`).MatchString(fp) {
		t.Fatalf("指纹格式：%q %v", fp, err)
	}

	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rk, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, raw := range map[string][]byte{
		"P-384": pemOf(t, &p384.PublicKey),
		"RSA":   pemOf(t, &rk.PublicKey),
		"非 PEM": []byte("hello"),
		"私钥块":   []byte(strings.Replace(string(pemOf(t, &k.PublicKey)), "PUBLIC KEY", "PRIVATE KEY", 2)),
	} {
		if _, err := ParsePublicKey(raw); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
}
