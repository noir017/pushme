package command

import (
	"context"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestPayloadFormat(t *testing.T) {
	c := Command{ID: "0123456789abcdef", To: "agent-grant", Issued: 1700000000, Expires: 1700000120, Body: "grant unraid,openwrt 1h"}
	want := "pushme-command-v1\n0123456789abcdef\nagent-grant\n1700000000\n1700000120\ngrant unraid,openwrt 1h"
	if got := string(Payload(c)); got != want {
		t.Fatalf("签名内容是 App 与 listen 共同的契约，改了两边都得改：\n%s", got)
	}
}

func TestCheck(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ok := Command{To: "a", ID: "0123456789abcdef", Issued: now.Unix(), Expires: now.Unix() + 120, Body: "grant unraid 1h", Sig: "c2ln"}
	if err := Check(ok, now); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Command){
		"缺收件方":   func(c *Command) { c.To = "" },
		"缺签名":    func(c *Command) { c.Sig = "" },
		"id 太短":  func(c *Command) { c.ID = "abc" },
		"正文多行":   func(c *Command) { c.Body = "grant\nrm" },
		"正文空":    func(c *Command) { c.Body = "" },
		"有效期太长":  func(c *Command) { c.Expires = c.Issued + 3600 },
		"过期在签发前": func(c *Command) { c.Expires = c.Issued - 1 },
		"已过期":    func(c *Command) { c.Issued, c.Expires = now.Unix()-200, now.Unix()-1 },
	} {
		c := ok
		mod(&c)
		if Check(c, now) == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
}

func TestBoxRouting(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	b := New(c.now)
	b.Add(Command{To: "a", ID: "1", Expires: c.t.Unix() + 60})
	b.Add(Command{To: "b", ID: "2", Expires: c.t.Unix() + 60})
	b.Add(Command{To: "a", ID: "3", Expires: c.t.Unix() + 60})
	got, cur := b.After("a", 0)
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" || cur != 3 {
		t.Fatalf("只给收件方自己的：%+v cur=%d", got, cur)
	}
	if got, _ := b.After("a", 1); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("after 之后的：%+v", got)
	}
	if got, _ := b.After("a", 99); len(got) != 2 {
		t.Fatalf("after 比最新还大（pushme 重启过）按 0 算：%+v", got)
	}
	c.t = c.t.Add(keepDone + 2*time.Minute)
	b.Add(Command{To: "a", ID: "4", Expires: c.t.Unix() + 60})
	if got, _ := b.After("a", 0); len(got) != 1 || got[0].ID != "4" {
		t.Fatalf("过期太久的被清掉：%+v", got)
	}
}

func TestBoxWaitWakes(t *testing.T) {
	b := New(nil)
	done := make(chan struct{})
	go func() { b.Wait(context.Background(), "a", 0, 10*time.Second); close(done) }()
	time.Sleep(50 * time.Millisecond)
	b.Add(Command{To: "b", ID: "x", Expires: time.Now().Unix() + 60})
	select {
	case <-done:
		t.Fatal("别人的指令不该唤醒")
	case <-time.After(200 * time.Millisecond):
	}
	b.Add(Command{To: "a", ID: "y", Expires: time.Now().Unix() + 60})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("自己的指令来了应立即醒")
	}
}

func TestListeners(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	b := New(c.now)
	b.Seen("agent-grant", "uniagent")
	b.Seen("hermes-grant", "")
	if l := b.Listeners(3 * time.Minute); len(l) != 2 || l[0].Label != "uniagent" || l[1].Label != "hermes-grant" {
		t.Fatalf("%+v", l)
	}
	c.t = c.t.Add(4 * time.Minute)
	b.Seen("hermes-grant", "hermes")
	if l := b.Listeners(3 * time.Minute); len(l) != 1 || l[0].Name != "hermes-grant" || l[0].Label != "hermes" {
		t.Fatalf("太久没来的不算在线：%+v", l)
	}
}
