// Package spool 是一个落盘的小重试队列：飞书暂时发不出去的消息先存进来，按退避时间表重试，
// 24 小时还没发出去就放弃并记日志。量很小（个人告警），所以整份 JSON 存一个文件、每次变更原子重写。
package spool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Backoff 是第 n 次重试前的等待（超出长度取最后一项）。
var Backoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// MaxAge 之后放弃。
const MaxAge = 24 * time.Hour

type Entry struct {
	ID       string    `json:"id"`
	Caller   string    `json:"caller"`
	Text     string    `json:"text"`
	Created  time.Time `json:"created"`
	Attempts int       `json:"attempts"`
	NextAt   time.Time `json:"next_at"`
	LastErr  string    `json:"last_err,omitempty"`
}

// SendFunc 发一条；返回的错误交给 Temporary 判断还要不要重试。
type SendFunc func(ctx context.Context, e Entry) (messageID string, err error)

type Spool struct {
	path      string
	send      SendFunc
	temporary func(error) bool
	now       func() time.Time
	log       *slog.Logger

	mu      sync.Mutex
	entries []Entry
	seq     int
}

// Open 读取已有的队列文件（不存在就当空队列）。
func Open(path string, send SendFunc, temporary func(error) bool, now func() time.Time, log *slog.Logger) (*Spool, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Spool{path: path, send: send, temporary: temporary, now: now, log: log}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(raw) > 0:
		if err := json.Unmarshal(raw, &s.entries); err != nil {
			return nil, fmt.Errorf("队列文件 %s 损坏: %w", path, err)
		}
	}
	return s, nil
}

func (s *Spool) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Add 把一条发送失败的消息入队，第一次重试在 Backoff[0] 之后。
func (s *Spool) Add(caller, text, lastErr string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.seq++
	e := Entry{
		ID:      fmt.Sprintf("%d-%d", now.UnixNano(), s.seq),
		Caller:  caller,
		Text:    text,
		Created: now,
		NextAt:  now.Add(Backoff[0]),
		LastErr: lastErr,
	}
	s.entries = append(s.entries, e)
	if err := s.saveLocked(); err != nil {
		s.entries = s.entries[:len(s.entries)-1]
		return "", err
	}
	return e.ID, nil
}

func (s *Spool) saveLocked() error {
	buf, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".spool-*")
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

// RetryDue 重试所有到期的条目（发送时不持锁，飞书慢不会卡住入队）。
func (s *Spool) RetryDue(ctx context.Context) {
	s.mu.Lock()
	now := s.now()
	var due []Entry
	for _, e := range s.entries {
		if !e.NextAt.After(now) {
			due = append(due, e)
		}
	}
	s.mu.Unlock()

	for _, e := range due {
		id, err := s.send(ctx, e)
		s.settle(e, id, err)
	}
}

func (s *Spool) settle(e Entry, messageID string, sendErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := -1
	for j := range s.entries {
		if s.entries[j].ID == e.ID {
			i = j
			break
		}
	}
	if i < 0 {
		return
	}
	now := s.now()
	cur := &s.entries[i]
	cur.Attempts++
	drop := false
	switch {
	case sendErr == nil:
		s.log.Info("spool delivered", "caller", e.Caller, "attempts", cur.Attempts, "msg_id", messageID)
		drop = true
	case !s.temporary(sendErr):
		s.log.Error("spool gave up (permanent error)", "caller", e.Caller, "attempts", cur.Attempts, "err", sendErr)
		drop = true
	case now.Sub(cur.Created) >= MaxAge:
		s.log.Error("spool gave up (too old)", "caller", e.Caller, "attempts", cur.Attempts, "err", sendErr)
		drop = true
	default:
		b := Backoff[min(cur.Attempts, len(Backoff)-1)]
		cur.NextAt = now.Add(b)
		cur.LastErr = sendErr.Error()
		s.log.Warn("spool retry failed", "caller", e.Caller, "attempts", cur.Attempts, "next_in", b.String(), "err", sendErr)
	}
	if drop {
		s.entries = append(s.entries[:i], s.entries[i+1:]...)
	}
	if err := s.saveLocked(); err != nil {
		s.log.Error("spool save failed", "err", err)
	}
}

// Run 每 interval 检查一次到期条目，直到 ctx 结束。
func (s *Spool) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RetryDue(ctx)
		}
	}
}
