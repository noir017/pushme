package command

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCheckRequest(t *testing.T) {
	ok := Request{Target: "awsfree2-grant", Hosts: "unraid,termux-mi14", Duration: "1h30m", Reason: "重启 newapi"}
	if err := CheckRequest(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Request{
		{Target: "", Hosts: "unraid", Duration: "1h", Reason: "x"},
		{Target: "t", Hosts: "unraid;rm", Duration: "1h", Reason: "x"},
		{Target: "t", Hosts: "Unraid", Duration: "1h", Reason: "x"},
		{Target: "t", Hosts: "unraid,", Duration: "1h", Reason: "x"},
		{Target: "t", Hosts: "unraid", Duration: "1d", Reason: "x"},
		{Target: "t", Hosts: "unraid", Duration: "", Reason: "x"},
		{Target: "t", Hosts: "unraid", Duration: "1h", Reason: " "},
		{Target: "t", Hosts: "unraid", Duration: "1h", Reason: "a\nb"},
	} {
		if CheckRequest(bad) == nil {
			t.Errorf("应拒绝：%+v", bad)
		}
	}
}

func TestRequestLifecycle(t *testing.T) {
	c := &clock{time.Unix(1_700_000_000, 0)}
	b := New(c.now)
	add := func() (Request, error) {
		return b.AddRequest(Request{Caller: "uniagent", Target: "agent-grant", Hosts: "unraid", Duration: "1h", Reason: "r"})
	}
	var ids []string
	for i := 0; i < MaxPendingRequests; i++ {
		r, err := add()
		if err != nil || r.State != RequestPending || r.Expires != c.t.Add(RequestTTL).Unix() || len(r.ID) != 24 {
			t.Fatalf("%+v %v", r, err)
		}
		ids = append(ids, r.ID)
	}
	if _, err := add(); !errors.Is(err, ErrTooManyRequests) {
		t.Fatalf("超过上限应拒：%v", err)
	}
	if _, err := b.AddRequest(Request{Caller: "hermes", Target: "t", Hosts: "all", Duration: "1h", Reason: "r"}); err != nil {
		t.Fatalf("上限按调用方算：%v", err)
	}
	if p := b.PendingRequests(); len(p) != 4 {
		t.Fatalf("%+v", p)
	}
	if r, err := b.AnswerRequest(ids[0], true, "phone"); err != nil || r.State != RequestApproved || r.By != "phone" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := b.AnswerRequest(ids[0], false, "phone"); !errors.Is(err, ErrAnswered) {
		t.Fatalf("只认第一次：%v", err)
	}
	if _, err := b.AnswerRequest("nope", true, "phone"); !errors.Is(err, ErrNoRequest) {
		t.Fatal(err)
	}
	// 处理掉一条，又能提
	if _, err := add(); err != nil {
		t.Fatal(err)
	}
	// 过期：pending 变 expired，处理不了；再过 keepDone 就删掉
	c.t = c.t.Add(RequestTTL)
	if r, _ := b.GetRequest(ids[1]); r.State != RequestExpired {
		t.Fatalf("%+v", r)
	}
	if _, err := b.AnswerRequest(ids[1], true, "phone"); !errors.Is(err, ErrAnswered) {
		t.Fatalf("过期了不能批：%v", err)
	}
	if p := b.PendingRequests(); len(p) != 0 {
		t.Fatalf("%+v", p)
	}
	c.t = c.t.Add(keepDone + time.Second)
	if _, ok := b.GetRequest(ids[0]); ok {
		t.Fatal("处理完太久的应删掉")
	}
}

func TestWaitRequestWakes(t *testing.T) {
	b := New(nil)
	r, _ := b.AddRequest(Request{Caller: "uniagent", Target: "t", Hosts: "unraid", Duration: "1h", Reason: "r"})
	done := make(chan struct{})
	go func() { b.WaitRequest(context.Background(), r.ID, 5*time.Second); close(done) }()
	time.Sleep(30 * time.Millisecond)
	if _, err := b.AnswerRequest(r.ID, false, "phone"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("处理后应唤醒")
	}
}
