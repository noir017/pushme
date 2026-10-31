// Package command 是「指令信箱」：手机 App 把签好名的指令投给某个调用方（按调用方名寻址），
// 调用方（宿主上的 pushme listen）长轮询取走，用自己钉死的手机公钥验签后执行。
//
// pushme 只存和转发，不验签：签名绑定了收件方、时间窗和指令内容，pushme 被攻破也伪造不出指令。
// 指令几分钟就过期，只放内存，pushme 重启就丢（listen 重连后从当前序号收起，不回放）。
package command

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// Keep 是信箱里最多留的条数；过期超过 keepDone 的也清掉。
	Keep     = 50
	keepDone = 10 * time.Minute
	// MaxLife 是一条指令从签发到过期最长多久。
	MaxLife = 5 * time.Minute
	// MaxBody 是指令正文上限（一行，如 "grant unraid,openwrt 1h"）。
	MaxBody = 200
)

var ErrInvalid = errors.New("invalid command")

type Command struct {
	Seq     int64  `json:"seq"`
	To      string `json:"to"`      // 收件的调用方名
	ID      string `json:"id"`      // 手机生成的随机串，防重放
	Issued  int64  `json:"issued"`  // 签发时刻（epoch 秒，手机按服务器时间校正过）
	Expires int64  `json:"expires"` // 过期时刻
	Body    string `json:"body"`
	Sig     string `json:"sig"` // 手机签名（base64 的 ASN.1 DER ECDSA），见 Payload
	By      string `json:"by"`  // 投递的接收方名
	TS      int64  `json:"ts"`  // pushme 受理时刻
}

// Payload 是手机签名的内容，listen 按同一规则拼出来验签（6 行，"\n" 分隔，末尾不带换行）：
//
//	pushme-command-v1 / id / to / issued / expires / body
func Payload(c Command) []byte {
	return []byte(strings.Join([]string{
		"pushme-command-v1", c.ID, c.To,
		strconv.FormatInt(c.Issued, 10), strconv.FormatInt(c.Expires, 10), c.Body,
	}, "\n"))
}

// Check 是投递时的形式检查（不验签）。
func Check(c Command, now time.Time) error {
	switch {
	case c.To == "" || c.ID == "" || c.Sig == "":
		return errors.New("to / id / sig are required")
	case len(c.ID) < 8 || len(c.ID) > 64 || strings.ContainsAny(c.ID, "\n "):
		return errors.New("id must be 8-64 chars without spaces")
	case c.Body == "" || len(c.Body) > MaxBody || strings.ContainsAny(c.Body, "\r\n"):
		return errors.New("body must be one line of 1-" + strconv.Itoa(MaxBody) + " bytes")
	case c.Expires <= c.Issued || time.Duration(c.Expires-c.Issued)*time.Second > MaxLife:
		return errors.New("expires must be within " + MaxLife.String() + " after issued")
	case c.Expires <= now.Unix():
		return errors.New("command already expired")
	}
	return nil
}

// Listener 是最近来取过指令的调用方，App 据此列出能下指令的对象。
type Listener struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	LastSeen int64  `json:"last_seen"`
}

type Box struct {
	now func() time.Time

	mu        sync.Mutex
	cmds      []Command
	seq       int64
	changed   chan struct{}
	listeners map[string]*Listener
}

func New(now func() time.Time) *Box {
	if now == nil {
		now = time.Now
	}
	return &Box{now: now, changed: make(chan struct{}), listeners: map[string]*Listener{}}
}

// Add 收下一条指令（调用方先 Check 过），编号并唤醒在等的 listen。
func (b *Box) Add(c Command) Command {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.seq++
	c.Seq, c.TS = b.seq, now.Unix()
	keep := b.cmds[:0]
	for _, x := range b.cmds {
		if now.Unix()-x.Expires < int64(keepDone/time.Second) {
			keep = append(keep, x)
		}
	}
	b.cmds = append(keep, c)
	if n := len(b.cmds) - Keep; n > 0 {
		b.cmds = append([]Command(nil), b.cmds[n:]...)
	}
	close(b.changed)
	b.changed = make(chan struct{})
	return c
}

func (b *Box) Latest() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// After 返回发给 to、序号大于 after 的指令，以及当前最新序号（下次的 after）。
// after 大于最新序号说明 pushme 重启过，这时按 0 算。
func (b *Box) After(to string, after int64) ([]Command, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if after > b.seq {
		after = 0
	}
	out := []Command{}
	for _, c := range b.cmds {
		if c.Seq > after && c.To == to {
			out = append(out, c)
		}
	}
	return out, b.seq
}

// Wait 在没有发给 to、序号大于 after 的指令时阻塞，直到来了、ctx 结束或 d 到期。
func (b *Box) Wait(ctx context.Context, to string, after int64, d time.Duration) {
	end := time.Now().Add(d)
	for {
		if cmds, _ := b.After(to, after); len(cmds) > 0 {
			return
		}
		b.mu.Lock()
		ch := b.changed
		b.mu.Unlock()
		left := time.Until(end)
		if left <= 0 {
			return
		}
		t := time.NewTimer(left)
		select {
		case <-ch:
			t.Stop()
		case <-t.C:
			return
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// Seen 记下某个调用方来取过指令。
func (b *Box) Seen(name, label string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if label == "" {
		label = name
	}
	b.listeners[name] = &Listener{Name: name, Label: label, LastSeen: b.now().Unix()}
}

// Listeners 返回 within 内来取过指令的调用方，按名字排序。
func (b *Box) Listeners(within time.Duration) []Listener {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().Unix()
	out := []Listener{}
	for _, l := range b.listeners {
		if now-l.LastSeen <= int64(within/time.Second) {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
