// Package inbox 是给手机 App 收消息用的收件箱：每条被受理的消息按到达顺序编号（序号连续递增）存一份，
// App 拿接收方 token 长轮询 GET /inbox?after=<序号>，有新消息立即返回。只留最近 Keep 条；
// 量很小（个人告警），所以整份 JSON 存一个文件、每次追加原子重写。
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Keep 是收件箱保留的条数，更早的丢弃（完整记录在飞书）。
const Keep = 200

type Message struct {
	Seq    int64  `json:"seq"`
	TS     int64  `json:"ts"` // 受理时刻，epoch 秒
	Caller string `json:"caller"`
	Title  string `json:"title"`
	Text   string `json:"text"`
}

type Inbox struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	msgs    []Message
	changed chan struct{} // 有新消息时关闭并换新，唤醒所有在等的长轮询
}

// Open 读取已有的收件箱文件（不存在就当空收件箱）。
func Open(path string, now func() time.Time) (*Inbox, error) {
	if now == nil {
		now = time.Now
	}
	b := &Inbox{path: path, now: now, changed: make(chan struct{})}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(raw) > 0:
		if err := json.Unmarshal(raw, &b.msgs); err != nil {
			return nil, fmt.Errorf("收件箱文件 %s 损坏: %w", path, err)
		}
	}
	return b, nil
}

func (b *Inbox) latestLocked() int64 {
	if len(b.msgs) == 0 {
		return 0
	}
	return b.msgs[len(b.msgs)-1].Seq
}

// Latest 返回最新一条的序号，空收件箱为 0。
func (b *Inbox) Latest() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.latestLocked()
}

// Add 存一条并唤醒在等的长轮询。落盘失败也照样留在内存里（App 照样收得到），错误交给调用方记日志。
func (b *Inbox) Add(caller, title, text string) (Message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := Message{Seq: b.latestLocked() + 1, TS: b.now().Unix(), Caller: caller, Title: title, Text: text}
	b.msgs = append(b.msgs, m)
	if n := len(b.msgs) - Keep; n > 0 {
		b.msgs = append([]Message(nil), b.msgs[n:]...)
	}
	close(b.changed)
	b.changed = make(chan struct{})
	return m, b.saveLocked()
}

// After 返回序号大于 after 的消息里最新的至多 limit 条（按序号升序）、没返回的较早条数（含已被丢弃的），
// 以及最新序号。after 大于最新序号说明收件箱被清空过，这时按 0 算：现存的都是清空之后的新消息。
func (b *Inbox) After(after int64, limit int) (msgs []Message, skipped int, latest int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	latest = b.latestLocked()
	if after > latest {
		after = 0
	}
	i := len(b.msgs)
	for i > 0 && b.msgs[i-1].Seq > after {
		i--
	}
	newer := b.msgs[i:]
	if len(newer) > limit {
		newer = newer[len(newer)-limit:]
	}
	msgs = append([]Message{}, newer...)
	return msgs, int(latest-after) - len(msgs), latest
}

// Wait 在没有序号大于 after 的消息时阻塞，直到来了新消息、ctx 结束或 d 到期。
// after 与最新序号不等（有新消息，或收件箱被清空过）时立即返回。
func (b *Inbox) Wait(ctx context.Context, after int64, d time.Duration) {
	b.mu.Lock()
	if b.latestLocked() != after {
		b.mu.Unlock()
		return
	}
	ch := b.changed
	b.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	}
}

func (b *Inbox) saveLocked() error {
	buf, err := json.Marshal(b.msgs)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(b.path), ".inbox-*")
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
	return os.Rename(tmp.Name(), b.path)
}
