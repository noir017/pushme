package spool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

var errTemp = errors.New("temporary")
var errPerm = errors.New("permanent")

func isTemp(err error) bool { return errors.Is(err, errTemp) }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAddPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.json")
	c := &clock{time.Unix(1_700_000_000, 0)}
	s, err := Open(path, nil, isTemp, c.now, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("openwrt", "hello", "boom"); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, nil, isTemp, c.now, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 1 || s2.entries[0].Text != "hello" || s2.entries[0].Caller != "openwrt" {
		t.Fatalf("重新打开后内容不对: %+v", s2.entries)
	}
}

func TestRetryDueHonoursBackoffAndDelivers(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	results := []error{errTemp, nil}
	calls := 0
	send := func(context.Context, Entry) (string, error) {
		err := results[calls]
		calls++
		return "om", err
	}
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"), send, isTemp, c.now, quiet())
	_, _ = s.Add("acme", "x", "")

	s.RetryDue(context.Background())
	if calls != 0 {
		t.Fatal("还没到第一次重试时间就发了")
	}
	c.t = c.t.Add(Backoff[0])
	s.RetryDue(context.Background()) // 第 1 次重试：临时失败
	if calls != 1 || s.Len() != 1 {
		t.Fatalf("calls=%d len=%d", calls, s.Len())
	}
	if want := c.t.Add(Backoff[1]); !s.entries[0].NextAt.Equal(want) {
		t.Fatalf("下次重试应在 %v，实际 %v", want, s.entries[0].NextAt)
	}
	c.t = c.t.Add(Backoff[1] - time.Second)
	s.RetryDue(context.Background())
	if calls != 1 {
		t.Fatal("退避时间没到就重试了")
	}
	c.t = c.t.Add(time.Second)
	s.RetryDue(context.Background()) // 第 2 次重试：成功
	if calls != 2 || s.Len() != 0 {
		t.Fatalf("成功后应出队: calls=%d len=%d", calls, s.Len())
	}
}

func TestPermanentErrorDrops(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	send := func(context.Context, Entry) (string, error) { return "", errPerm }
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"), send, isTemp, c.now, quiet())
	_, _ = s.Add("q", "x", "")
	c.t = c.t.Add(Backoff[0])
	s.RetryDue(context.Background())
	if s.Len() != 0 {
		t.Fatal("永久错误应直接丢弃")
	}
}

func TestGivesUpAfterMaxAge(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	send := func(context.Context, Entry) (string, error) { return "", errTemp }
	s, _ := Open(filepath.Join(t.TempDir(), "s.json"), send, isTemp, c.now, quiet())
	_, _ = s.Add("q", "x", "")
	for i := 0; i < 40 && s.Len() > 0; i++ {
		c.t = c.t.Add(time.Hour)
		s.RetryDue(context.Background())
	}
	if s.Len() != 0 {
		t.Fatal("超过 MaxAge 应放弃")
	}
	if c.t.Sub(time.Unix(1_700_000_000, 0)) < MaxAge {
		t.Fatal("放弃得太早")
	}
}
