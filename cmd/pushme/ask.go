package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/noir017/pushme/internal/approval"
)

// pushme ask：发起一条审批，等手机答复，用钉死的手机公钥验签。给宿主上的脚本用（如 agent-grant）：
//
//	PUSHME_URL=http://192.168.0.185:8290 PUSHME_TOKEN=<调用方 token> \
//	  pushme ask -pubkey /path/approver.pub -title "uniagent 申请提权" -text "unraid（root）1 小时" [-ttl 5m]
//
// stdout 打一行结果：approved / denied / expired / bad-signature。
// 退出码：0 批准且签名有效；1 拒绝、过期或签名不对；2 用法、配置或网络出错。
// 验签用的是本机发出去的 title/text 和创建时拿到的 id/challenge/expires，不信 pushme 事后返回的任何内容。

const (
	resApproved     = "approved"
	resDenied       = "denied"
	resExpired      = "expired"
	resBadSignature = "bad-signature"

	askPollWait = 50               // 每次长轮询挂多少秒（pushme 上限 60）
	askGrace    = 60 * time.Second // 过了 expires 再等多久（两边时钟差、最后一次答复在路上）
)

type askConfig struct {
	URL, Token  string
	Pub         *ecdsa.PublicKey
	Title, Text string
	TTL         time.Duration
	Client      *http.Client
	Log         io.Writer       // 等待过程中的提示（网络抖动重试等）
	Backoff     []time.Duration // 连不上 pushme 时的重试间隔，逐次取，用完一直用最后一个
}

func askMain(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pushme ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", getenv("PUSHME_URL"), "pushme 根地址（默认取 $PUSHME_URL）")
	pubPath := fs.String("pubkey", "", "手机公钥文件（PEM），必填")
	title := fs.String("title", "", "标题（单独一行显示）")
	text := fs.String("text", "", "正文")
	ttl := fs.Duration("ttl", approval.DefaultTTL, "多久没人答复就算拒绝（30s～1h）")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "用法：PUSHME_TOKEN=<调用方 token> pushme ask -pubkey FILE -title T -text X [-url URL] [-ttl 5m]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := strings.TrimSpace(getenv("PUSHME_TOKEN"))
	switch {
	case *base == "":
		fmt.Fprintln(stderr, "pushme ask: 没有 pushme 地址（-url 或 $PUSHME_URL）")
		return 2
	case token == "":
		fmt.Fprintln(stderr, "pushme ask: 没有 $PUSHME_TOKEN")
		return 2
	case *pubPath == "":
		fmt.Fprintln(stderr, "pushme ask: 缺 -pubkey")
		return 2
	case strings.TrimSpace(*title) == "" && strings.TrimSpace(*text) == "":
		fmt.Fprintln(stderr, "pushme ask: -title 和 -text 至少要一个")
		return 2
	case !utf8.ValidString(*title) || !utf8.ValidString(*text):
		// JSON 编码会把坏字节换成 U+FFFD，手机签的就不是这里的原文了
		fmt.Fprintln(stderr, "pushme ask: -title / -text 不是合法的 UTF-8")
		return 2
	}
	raw, err := os.ReadFile(*pubPath)
	if err != nil {
		fmt.Fprintln(stderr, "pushme ask:", err)
		return 2
	}
	pub, err := approval.ParsePublicKey(raw)
	if err != nil {
		fmt.Fprintf(stderr, "pushme ask: %s: %v\n", *pubPath, err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	res, err := runAsk(ctx, askConfig{
		URL: strings.TrimRight(*base, "/"), Token: token, Pub: pub,
		Title: *title, Text: *text, TTL: *ttl, Log: stderr,
	})
	if err != nil {
		fmt.Fprintln(stderr, "pushme ask:", err)
		return 2
	}
	fmt.Fprintln(stdout, res)
	if res == resApproved {
		return 0
	}
	return 1
}

// runAsk 发起并等到终态，返回 resApproved / resDenied / resExpired / resBadSignature；
// 发不出去、或一直连不上 pushme 直到过期还没拿到结果时返回 error。
func runAsk(ctx context.Context, cfg askConfig) (string, error) {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: time.Duration(askPollWait+20) * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}
	}

	var created struct {
		ID        string `json:"id"`
		Challenge string `json:"challenge"`
		Expires   int64  `json:"expires"`
	}
	body, _ := json.Marshal(map[string]any{"title": cfg.Title, "text": cfg.Text, "ttl": int(cfg.TTL / time.Second)})
	if err := askCall(ctx, cfg, "POST", "/approvals", body, &created); err != nil {
		return "", fmt.Errorf("发起审批失败: %w", err)
	}
	if created.ID == "" || created.Challenge == "" || created.Expires == 0 {
		return "", errors.New("发起审批失败: pushme 的响应缺 id/challenge/expires")
	}
	mine := approval.Approval{
		ID: created.ID, Challenge: created.Challenge, Expires: created.Expires,
		Title: cfg.Title, Text: cfg.Text,
	}
	deadline := time.Unix(created.Expires, 0).Add(askGrace)

	failures := 0
	for {
		var got struct {
			State string `json:"state"`
			Sig   string `json:"sig"`
		}
		path := "/approvals/" + url.PathEscape(mine.ID) + "?wait=" + strconv.Itoa(askPollWait)
		err := askCall(ctx, cfg, "GET", path, nil, &got)
		var he *httpError
		switch {
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.As(err, &he) && he.status == http.StatusNotFound:
			// pushme 丢了这条（重启时没读到审批文件之类），当作没批
			return "", errors.New("pushme 上找不到这条审批了")
		case errors.As(err, &he) && he.status < 500:
			return "", fmt.Errorf("取审批结果失败: %w", err)
		case err != nil:
			if !time.Now().Before(deadline) {
				return "", fmt.Errorf("一直连不上 pushme，审批已过期: %w", err)
			}
			wait := cfg.Backoff[min(failures, len(cfg.Backoff)-1)]
			failures++
			fmt.Fprintf(cfg.Log, "pushme ask: 等结果时出错，%s 后重试: %v\n", wait, err)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		switch got.State {
		case approval.Approved:
			mine.Sig = got.Sig
			if approval.Verify(cfg.Pub, mine) != nil {
				return resBadSignature, nil
			}
			return resApproved, nil
		case approval.Denied:
			return resDenied, nil
		case approval.Expired:
			return resExpired, nil
		case approval.Pending:
			if !time.Now().Before(deadline) {
				return resExpired, nil
			}
		default:
			return "", fmt.Errorf("pushme 返回了不认识的状态 %q", got.State)
		}
	}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.msg) }

func askCall(ctx context.Context, cfg askConfig, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, cfg.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &httpError{status: res.StatusCode, msg: e.Error}
	}
	return json.Unmarshal(raw, out)
}

// pushme fingerprint <pub.pem>：打印手机公钥的指纹，配对时与 App 上显示的核对。
func fingerprintMain(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "用法：pushme fingerprint <公钥文件.pem>")
		return 2
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "pushme fingerprint:", err)
		return 2
	}
	pub, err := approval.ParsePublicKey(raw)
	if err != nil {
		fmt.Fprintf(stderr, "pushme fingerprint: %s: %v\n", args[0], err)
		return 2
	}
	fp, err := approval.Fingerprint(pub)
	if err != nil {
		fmt.Fprintln(stderr, "pushme fingerprint:", err)
		return 2
	}
	fmt.Fprintln(stdout, fp)
	return 0
}
