// Package feishu 用飞书「应用机器人」给一个固定收件人发文本消息。
//
// 流程：app_id + app_secret 换 tenant_access_token（缓存到过期前 5 分钟），
// 再 POST /open-apis/im/v1/messages。逻辑照 quantlab-agents 的 notify.js，
// 但修了它的一个隐患：先读响应体里的 code，再看 HTTP 状态 —— 飞书的 token
// 失效码有时配着非 200 状态，只看状态会跳过「清缓存重试」。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const DefaultBaseURL = "https://open.feishu.cn"

// token 失效/无效：清缓存、重新换一次再发。
var invalidTokenCodes = map[int]bool{99991661: true, 99991663: true, 99991668: true}

// 飞书侧限流：值得稍后重试。
var temporaryCodes = map[int]bool{99991400: true}

// Receiver 是飞书的 receive_id_type + receive_id。
type Receiver struct {
	IDType string
	ID     string
}

// ParseReceiver 解析 `user:ou_xxx` / `chat:oc_xxx` / `email:a@b` 这种写法（与 hermes push.py、
// quantlab 的 QLA_NOTIFY_FEISHU_TO 同语法）。认不出就报错，不猜默认值。
func ParseReceiver(s string) (Receiver, error) {
	kind, id, ok := strings.Cut(strings.TrimSpace(s), ":")
	id = strings.TrimSpace(id)
	if !ok || id == "" {
		return Receiver{}, fmt.Errorf("收件人格式应为 user:ou_xxx / chat:oc_xxx / email:xxx")
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "user", "open_id":
		return Receiver{"open_id", id}, nil
	case "chat", "chat_id":
		return Receiver{"chat_id", id}, nil
	case "email":
		return Receiver{"email", id}, nil
	}
	return Receiver{}, fmt.Errorf("不认识的收件人类型 %q（只支持 user / chat / email）", kind)
}

// Kind 返回收件人类型的简称，用于日志（不暴露 id）。
func (r Receiver) Kind() string { return r.IDType }

// Error 是一次失败的发送。Temporary 为 true 时值得稍后重试（网络、5xx、429、飞书限流）。
type Error struct {
	Op         string
	HTTPStatus int
	Code       int
	Msg        string
	Temporary  bool
	Err        error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("feishu " + e.Op)
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, ": http %d", e.HTTPStatus)
	}
	if e.Code != 0 {
		fmt.Fprintf(&b, ": code %d %s", e.Code, e.Msg)
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// IsTemporary 判断 err 是否值得稍后重试。非 *Error 的错误（例如 ctx 超时）按临时处理。
func IsTemporary(err error) bool {
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Temporary
	}
	return err != nil
}

// Client 是线程安全的，可在多个 goroutine 间共享。
type Client struct {
	BaseURL   string
	AppID     string
	AppSecret string
	To        Receiver
	HTTP      *http.Client
	Now       func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 8 * time.Second}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

// apiResp 覆盖两个接口共用的字段。
type apiResp struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
	Data              struct {
		MessageID string `json:"message_id"`
	} `json:"data"`
}

// post 发一个 JSON 请求，返回解析后的响应。判定顺序：网络错误 → 响应体 code → HTTP 状态。
func (c *Client) post(ctx context.Context, op, path, bearer string, body any) (*apiResp, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Op: op, Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+path, bytes.NewReader(buf))
	if err != nil {
		return nil, &Error{Op: op, Err: err}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil, &Error{Op: op, Temporary: true, Err: err}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	var r apiResp
	if jerr := json.Unmarshal(raw, &r); jerr != nil {
		return nil, &Error{Op: op, HTTPStatus: res.StatusCode, Temporary: res.StatusCode >= 500 || res.StatusCode == 429,
			Err: fmt.Errorf("响应不是 JSON: %.120q", raw)}
	}
	if r.Code != 0 {
		return &r, &Error{Op: op, HTTPStatus: res.StatusCode, Code: r.Code, Msg: r.Msg,
			Temporary: temporaryCodes[r.Code] || res.StatusCode >= 500 || res.StatusCode == 429}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &r, &Error{Op: op, HTTPStatus: res.StatusCode, Temporary: res.StatusCode >= 500 || res.StatusCode == 429}
	}
	return &r, nil
}

func (c *Client) tenantToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires) {
		return c.token, nil
	}
	r, err := c.post(ctx, "tenant_access_token", "/open-apis/auth/v3/tenant_access_token/internal", "",
		map[string]string{"app_id": c.AppID, "app_secret": c.AppSecret})
	if err != nil {
		return "", err
	}
	if r.TenantAccessToken == "" {
		return "", &Error{Op: "tenant_access_token", Err: errors.New("响应里没有 tenant_access_token")}
	}
	ttl := time.Duration(r.Expire) * time.Second
	if ttl <= 0 {
		ttl = 7200 * time.Second
	}
	// 提前 5 分钟换；ttl 本身不到 10 分钟时按一半算，免得缓存一个马上过期的 token。
	if ttl > 10*time.Minute {
		ttl -= 5 * time.Minute
	} else {
		ttl /= 2
	}
	c.token, c.expires = r.TenantAccessToken, c.now().Add(ttl)
	return c.token, nil
}

func (c *Client) dropToken() {
	c.mu.Lock()
	c.token, c.expires = "", time.Time{}
	c.mu.Unlock()
}

// SendText 给 c.To 发一条文本消息，返回飞书的 message_id。
// token 失效码只清缓存重试一次；其余错误原样返回，由调用方按 IsTemporary 决定是否排队重试。
func (c *Client) SendText(ctx context.Context, text string) (string, error) {
	content, _ := json.Marshal(map[string]string{"text": text})
	body := map[string]string{"receive_id": c.To.ID, "msg_type": "text", "content": string(content)}
	path := "/open-apis/im/v1/messages?receive_id_type=" + c.To.IDType

	for attempt := 0; ; attempt++ {
		tok, err := c.tenantToken(ctx)
		if err != nil {
			return "", err
		}
		r, err := c.post(ctx, "send", path, tok, body)
		if err == nil {
			return r.Data.MessageID, nil
		}
		var fe *Error
		if attempt == 0 && errors.As(err, &fe) && invalidTokenCodes[fe.Code] {
			c.dropToken()
			continue
		}
		return "", err
	}
}
