// Package approval 是审批（手机确认）的存储与签名约定：调用方发起一条审批，手机 App 看完内容、
// 用本机密钥签名后答复，发起方取回结果、用自己钉死的手机公钥验签。
//
// pushme 只存和转发，不验签、不判真假：pushme 被攻破也伪造不出「批准」，因为签名要手机里的私钥。
// 量很小（同时挂着的不过几条），整份 JSON 存一个文件、每次变化原子重写。
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 审批的状态。pending 之外都是终态。
const (
	Pending  = "pending"
	Approved = "approved"
	Denied   = "denied"
	Expired  = "expired"
)

const (
	DefaultTTL = 5 * time.Minute
	MinTTL     = 30 * time.Second
	MaxTTL     = time.Hour
	// MaxPending 是每个调用方同时挂着的审批上限，防止刷屏让人误点。
	MaxPending = 3
	// keepDone 是终态的审批再留多久，供发起方取结果（它可能正好断线重连）。
	keepDone = 10 * time.Minute
)

var (
	ErrNotFound   = errors.New("no such approval")
	ErrTooMany    = fmt.Errorf("too many pending approvals (max %d per caller)", MaxPending)
	ErrNotPending = errors.New("approval is no longer pending")
)

type Approval struct {
	ID        string `json:"id"`
	Caller    string `json:"caller"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	Challenge string `json:"challenge"`
	Created   int64  `json:"created"` // epoch 秒
	Expires   int64  `json:"expires"` // epoch 秒，到点没人答复即为 expired
	State     string `json:"state"`
	Sig       string `json:"sig,omitempty"`      // 批准时手机的签名（base64 的 ASN.1 DER ECDSA），见 Payload
	Answered  int64  `json:"answered,omitempty"` // 答复时刻
	By        string `json:"by,omitempty"`       // 答复的接收方名
}

type Store struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	items   map[string]*Approval
	changed chan struct{} // 有答复时关闭并换新，唤醒所有在等的发起方
}

// Open 读取已有的审批文件（不存在就当空的）。重启前挂着的审批照样有效，到点照样过期。
func Open(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, now: now, items: map[string]*Approval{}, changed: make(chan struct{})}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(raw) > 0:
		var list []*Approval
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("审批文件 %s 损坏: %w", path, err)
		}
		for _, a := range list {
			s.items[a.ID] = a
		}
	}
	return s, nil
}

// tidyLocked 把到点的 pending 标成 expired，删掉终态超过 keepDone 的。
func (s *Store) tidyLocked() {
	now := s.now().Unix()
	for id, a := range s.items {
		if a.State == Pending && now >= a.Expires {
			a.State = Expired
		}
		done := a.Answered
		if a.State == Expired {
			done = a.Expires
		}
		if a.State != Pending && now-done >= int64(keepDone/time.Second) {
			delete(s.items, id)
		}
	}
}

// Create 发起一条审批。ttl 超出 [MinTTL, MaxTTL] 时截到边界，0 表示 DefaultTTL。
// 标题、正文原样保存（不截断、不改写）：发起方按自己发出的内容验签，改一个字节都验不过。
func (s *Store) Create(caller, title, text string, ttl time.Duration) (Approval, error) {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	ttl = min(max(ttl, MinTTL), MaxTTL)
	id, err := randHex(8)
	if err != nil {
		return Approval{}, err
	}
	challenge, err := randHex(16)
	if err != nil {
		return Approval{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tidyLocked()
	pending := 0
	for _, a := range s.items {
		if a.Caller == caller && a.State == Pending {
			pending++
		}
	}
	if pending >= MaxPending {
		return Approval{}, ErrTooMany
	}
	now := s.now()
	a := &Approval{
		ID: id, Caller: caller, Title: title, Text: text, Challenge: challenge,
		Created: now.Unix(), Expires: now.Add(ttl).Unix(), State: Pending,
	}
	s.items[id] = a
	return *a, s.saveLocked()
}

// Get 返回一条审批的当前状态。
func (s *Store) Get(id string) (Approval, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tidyLocked()
	a, ok := s.items[id]
	if !ok {
		return Approval{}, false
	}
	return *a, true
}

// Pending 返回所有还在等答复的审批，按发起时间升序。
func (s *Store) Pending() []Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tidyLocked()
	out := []Approval{}
	for _, a := range s.items {
		if a.State == Pending {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Answer 记下手机的答复，只认第一次。批准时 sig 是手机的签名（这里不验，只存）。
func (s *Store) Answer(id, by string, approve bool, sig string) (Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tidyLocked()
	a, ok := s.items[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	if a.State != Pending {
		return *a, ErrNotPending
	}
	a.State, a.Answered, a.By = Denied, s.now().Unix(), by
	if approve {
		a.State, a.Sig = Approved, sig
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return *a, s.saveLocked()
}

// Wait 在审批还是 pending 时阻塞，直到有答复、到期、ctx 结束或 d 到期。
func (s *Store) Wait(ctx context.Context, id string, d time.Duration) {
	end := time.Now().Add(d)
	for {
		s.mu.Lock()
		s.tidyLocked()
		a, ok := s.items[id]
		if !ok || a.State != Pending {
			s.mu.Unlock()
			return
		}
		ch := s.changed
		// 按注入的时钟算到期还剩多久，到点醒来把它标成 expired
		left := min(time.Until(end), time.Duration(a.Expires-s.now().Unix())*time.Second)
		s.mu.Unlock()
		if left <= 0 {
			return
		}
		t := time.NewTimer(left)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
		if !time.Now().Before(end) {
			return
		}
	}
}

func (s *Store) saveLocked() error {
	list := make([]*Approval, 0, len(s.items))
	for _, a := range s.items {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created < list[j].Created })
	buf, err := json.Marshal(list)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".approvals-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
