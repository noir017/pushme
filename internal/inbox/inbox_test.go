package inbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T, path string) *Inbox {
	t.Helper()
	b, err := Open(path, func() time.Time { return time.Unix(1_700_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAddAfterAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")
	b := open(t, path)
	if b.Latest() != 0 {
		t.Fatalf("空收件箱最新序号应为 0，实际 %d", b.Latest())
	}
	for _, text := range []string{"a", "b", "c"} {
		if _, err := b.Add("openwrt", "", text); err != nil {
			t.Fatal(err)
		}
	}
	msgs, skipped, latest := b.After(1, 10)
	if len(msgs) != 2 || msgs[0].Seq != 2 || msgs[1].Text != "c" || skipped != 0 || latest != 3 {
		t.Fatalf("%+v skipped=%d latest=%d", msgs, skipped, latest)
	}
	if msgs[0].TS != 1_700_000_000 || msgs[0].Caller != "openwrt" {
		t.Fatalf("%+v", msgs[0])
	}

	// 重启后序号接着往下编
	b2 := open(t, path)
	m, _ := b2.Add("acme", "续期", "ok")
	if m.Seq != 4 || b2.Latest() != 4 {
		t.Fatalf("重启后序号应接着编：%+v", m)
	}
}

func TestAfterReturnsNewestWithinLimit(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "inbox.json"))
	for i := 0; i < 10; i++ {
		_, _ = b.Add("c", "", "x")
	}
	msgs, skipped, latest := b.After(2, 3)
	if len(msgs) != 3 || msgs[0].Seq != 8 || msgs[2].Seq != 10 || skipped != 5 || latest != 10 {
		t.Fatalf("应返回最新 3 条、跳过 5 条：%+v skipped=%d", msgs, skipped)
	}
}

func TestKeepDropsOldestAndCountsThemAsSkipped(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "inbox.json"))
	for i := 0; i < Keep+5; i++ {
		_, _ = b.Add("c", "", "x")
	}
	msgs, skipped, latest := b.After(0, Keep+10)
	if len(msgs) != Keep || msgs[0].Seq != 6 || skipped != 5 || latest != Keep+5 {
		t.Fatalf("len=%d first=%d skipped=%d latest=%d", len(msgs), msgs[0].Seq, skipped, latest)
	}
}

func TestAfterBeyondLatestMeansInboxWasReset(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "inbox.json"))
	_, _ = b.Add("c", "", "清空后的第一条")
	msgs, skipped, latest := b.After(500, 10)
	if len(msgs) != 1 || msgs[0].Seq != 1 || skipped != 0 || latest != 1 {
		t.Fatalf("收件箱清空过，应从头给：%+v skipped=%d latest=%d", msgs, skipped, latest)
	}
}

func TestWaitWakesOnAdd(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "inbox.json"))
	_, _ = b.Add("c", "", "旧的")
	done := make(chan time.Duration)
	go func() {
		start := time.Now()
		b.Wait(context.Background(), 1, 5*time.Second)
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	_, _ = b.Add("c", "", "新的")
	if d := <-done; d > 2*time.Second {
		t.Fatalf("来了新消息应立即唤醒，等了 %v", d)
	}
}

func TestWaitReturnsAtOnceOrOnTimeoutOrCancel(t *testing.T) {
	b := open(t, filepath.Join(t.TempDir(), "inbox.json"))
	_, _ = b.Add("c", "", "x")

	start := time.Now()
	b.Wait(context.Background(), 0, 5*time.Second) // 已有更新的
	b.Wait(context.Background(), 9, 5*time.Second) // 收件箱清空过
	if time.Since(start) > time.Second {
		t.Fatal("有新消息或收件箱清空过时应立即返回")
	}

	start = time.Now()
	b.Wait(context.Background(), 1, 100*time.Millisecond)
	if d := time.Since(start); d < 80*time.Millisecond {
		t.Fatalf("没有新消息应等到超时，只等了 %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start = time.Now()
	b.Wait(ctx, 1, 5*time.Second)
	if time.Since(start) > 2*time.Second {
		t.Fatal("ctx 结束（服务关闭）应立即返回")
	}
}

func TestCorruptFileRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, nil); err == nil {
		t.Fatal("损坏的文件应报错")
	}
}
