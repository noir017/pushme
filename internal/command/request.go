package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 提权申请：agent（调用方）想要某几台机器的提权时，向 pushme 提一条申请，App 上列出来。
// 批准不经 pushme：App 照申请内容签一条授权指令（Command）发给 Target，由那边验签执行；
// pushme 只记下「批了 / 拒了」好让 agent 知道结果。所以 pushme 被攻破也只能谎报结果，批不出授权。

const (
	// RequestTTL 是申请多久没人处理就过期。
	RequestTTL = 5 * time.Minute
	// MaxPendingRequests 是每个调用方同时挂着的申请上限。
	MaxPendingRequests = 3
	// MaxReason 是申请理由上限（字节）。
	MaxReason = 300

	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
	RequestExpired  = "expired"
)

var (
	ErrTooManyRequests = errors.New("too many pending requests")
	ErrNoRequest       = errors.New("no such request")
	ErrAnswered        = errors.New("request already answered or expired")

	reHosts    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(,[a-z0-9][a-z0-9_-]*)*$`)
	reDuration = regexp.MustCompile(`^([0-9]+h)?([0-9]+m)?$`)
)

type Request struct {
	ID       string `json:"id"`
	Caller   string `json:"caller"`   // 发申请的调用方（agent）
	Target   string `json:"target"`   // 批准时授权指令发给谁（跑着 pushme listen 的调用方名）
	Hosts    string `json:"hosts"`    // 机器名，逗号分隔；all 为全部
	Duration string `json:"duration"` // 如 1h、30m
	Reason   string `json:"reason"`
	Created  int64  `json:"created"`
	Expires  int64  `json:"expires"`
	State    string `json:"state"`
	By       string `json:"by,omitempty"`       // 处理它的接收方
	Answered int64  `json:"answered,omitempty"` // 处理时刻
}

// CheckRequest 是提交时的形式检查。
func CheckRequest(r Request) error {
	switch {
	case r.Target == "":
		return errors.New("target is required")
	case len(r.Hosts) > 100 || !reHosts.MatchString(r.Hosts):
		return errors.New("hosts must be comma-separated machine names (or all)")
	case r.Duration == "" || len(r.Duration) > 8 || !reDuration.MatchString(r.Duration):
		return errors.New("duration must look like 30m / 2h / 1h30m")
	case strings.TrimSpace(r.Reason) == "" || len(r.Reason) > MaxReason || strings.ContainsAny(r.Reason, "\r\n"):
		return errors.New("reason must be one non-empty line of at most 300 bytes")
	}
	return nil
}

// AddRequest 收下一条申请（调用方先 CheckRequest 过），编上随机 id。
func (b *Box) AddRequest(r Request) (Request, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.expireRequestsLocked(now)
	pending := 0
	for _, x := range b.requests {
		if x.Caller == r.Caller && x.State == RequestPending {
			pending++
		}
	}
	if pending >= MaxPendingRequests {
		return Request{}, ErrTooManyRequests
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	r.ID = hex.EncodeToString(id)
	r.Created, r.Expires, r.State = now.Unix(), now.Add(RequestTTL).Unix(), RequestPending
	r.By, r.Answered = "", 0
	b.requests[r.ID] = &r
	b.notifyRequestsLocked()
	return r, nil
}

// PendingRequests 返回还在等处理的申请，早的在前。
func (b *Box) PendingRequests() []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireRequestsLocked(b.now())
	out := []Request{}
	for _, r := range b.requests {
		if r.State == RequestPending {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

// GetRequest 取一条申请（含已处理、已过期的，保留 keepDone）。
func (b *Box) GetRequest(id string) (Request, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireRequestsLocked(b.now())
	r, ok := b.requests[id]
	if !ok {
		return Request{}, false
	}
	return *r, true
}

// AnswerRequest 记下处理结果：approved 或 denied。只认第一次，过期了也不认。
func (b *Box) AnswerRequest(id string, approve bool, by string) (Request, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.expireRequestsLocked(now)
	r, ok := b.requests[id]
	if !ok {
		return Request{}, ErrNoRequest
	}
	if r.State != RequestPending {
		return *r, ErrAnswered
	}
	r.State = RequestDenied
	if approve {
		r.State = RequestApproved
	}
	r.By, r.Answered = by, now.Unix()
	b.notifyRequestsLocked()
	return *r, nil
}

// WaitRequest 在申请还是 pending 时阻塞，直到有结果、过期、ctx 结束或 d 到期。
func (b *Box) WaitRequest(ctx context.Context, id string, d time.Duration) {
	end := time.Now().Add(d)
	for {
		b.mu.Lock()
		b.expireRequestsLocked(b.now())
		r, ok := b.requests[id]
		pending, expires := ok && r.State == RequestPending, int64(0)
		if ok {
			expires = r.Expires
		}
		ch := b.reqChanged
		b.mu.Unlock()
		if !pending {
			return
		}
		// 过期没有事件唤醒：最多等到它的过期时刻
		left := min(time.Until(end), time.Until(time.Unix(expires, 0))+time.Second)
		if left <= 0 {
			return
		}
		t := time.NewTimer(left)
		select {
		case <-ch:
			t.Stop()
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// expireRequestsLocked 把到期的 pending 标成 expired，删掉处理完超过 keepDone 的。调用方持有 b.mu。
func (b *Box) expireRequestsLocked(now time.Time) {
	changed := false
	for id, r := range b.requests {
		if r.State == RequestPending && now.Unix() >= r.Expires {
			r.State, changed = RequestExpired, true
		}
		done := r.Answered
		if done == 0 {
			done = r.Expires
		}
		if r.State != RequestPending && now.Unix()-done > int64(keepDone/time.Second) {
			delete(b.requests, id)
		}
	}
	if changed {
		b.notifyRequestsLocked()
	}
}

func (b *Box) notifyRequestsLocked() {
	close(b.reqChanged)
	b.reqChanged = make(chan struct{})
}
