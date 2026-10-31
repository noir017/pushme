package main

import (
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
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noir017/pushme/internal/approval"
	"github.com/noir017/pushme/internal/command"
)

// pushme listen：长轮询取手机发给自己的指令，用钉死的手机公钥验签，验过就执行。给宿主上的脚本用：
//
//	PUSHME_URL=http://192.168.0.185:8290 PUSHME_TOKEN=<调用方 token> \
//	  pushme listen -to agent-grant -label uniagent -pubkey /etc/agent-grant/approver.pub \
//	                -exec /usr/local/sbin/agent-grant -exec-arg _cmd -status-arg _state
//
// 指令正文按空白切开，接在 -exec（和 -exec-arg）后面当参数执行（不经 shell），环境变量另带 PUSHME_CMD_ID / PUSHME_CMD_BY。
// 只执行：收件方是 -to、签名对得上、没过期（签发时刻不晚于现在 1 分钟、有效期不超过 5 分钟）、同一个 id 没执行过的。
// 验不过的不执行，经 /send 推一条警告。一直跑到 SIGINT/SIGTERM；连不上 pushme 就退避重试，token 不对（4xx）直接退出。
//
// 给了 -status-arg 时，每 30 秒和每执行完一条指令，跑一次 -exec 加这些参数（不带 -exec-arg），
// 输出（一个 JSON 对象，如当前授权）经 POST /commands/status 报给 pushme，App 首页显示。

type listenConfig struct {
	URL, Token, To, Label string
	Pub                   *ecdsa.PublicKey
	Exec                  string
	ExecArgs              []string
	// StatusArgs 非空时定时跑 Exec StatusArgs…，把输出当状态上报，见 reportStatus。
	StatusArgs  []string
	StatusEvery time.Duration
	Client      *http.Client
	Log         io.Writer
	Now         func() time.Time
	Backoff     []time.Duration
	// Run 执行一条验过的指令，返回合并的输出；默认 exec.CommandContext，2 分钟超时。
	Run func(ctx context.Context, name string, args, env []string) (string, error)
}

const (
	listenPollWait = 50
	statusEvery    = 30 * time.Second
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, " ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func listenMain(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pushme listen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("url", getenv("PUSHME_URL"), "pushme 根地址（默认取 $PUSHME_URL）")
	to := fs.String("to", "", "本调用方在 pushme 里的名字（指令里签了收件方，必须一致），必填")
	label := fs.String("label", "", "在 App 上显示的名字（默认同 -to）")
	pubPath := fs.String("pubkey", "", "手机公钥文件（PEM），必填")
	execPath := fs.String("exec", "", "验过的指令交给它执行，必填")
	var execArgs multiFlag
	fs.Var(&execArgs, "exec-arg", "放在指令正文前面的固定参数（可重复）")
	var statusArgs multiFlag
	fs.Var(&statusArgs, "status-arg", "查当前状态时接在 -exec 后面的参数（可重复；给了才上报，输出要是一个 JSON 对象）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	token := strings.TrimSpace(getenv("PUSHME_TOKEN"))
	if *base == "" || token == "" || *to == "" || *pubPath == "" || *execPath == "" {
		fmt.Fprintln(stderr, "用法：PUSHME_URL=… PUSHME_TOKEN=… pushme listen -to NAME -pubkey FILE -exec CMD [-exec-arg ARG]… [-label LABEL]")
		return 2
	}
	raw, err := os.ReadFile(*pubPath)
	if err != nil {
		fmt.Fprintln(stderr, "pushme listen:", err)
		return 2
	}
	pub, err := approval.ParsePublicKey(raw)
	if err != nil {
		fmt.Fprintf(stderr, "pushme listen: %s: %v\n", *pubPath, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err = runListen(ctx, listenConfig{
		URL: strings.TrimRight(*base, "/"), Token: token, To: *to, Label: *label,
		Pub: pub, Exec: *execPath, ExecArgs: execArgs, StatusArgs: statusArgs, Log: stdout,
	})
	if err != nil {
		fmt.Fprintln(stderr, "pushme listen:", err)
		return 1
	}
	return 0
}

func runListen(ctx context.Context, cfg listenConfig) error {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: time.Duration(listenPollWait+20) * time.Second}
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Label == "" {
		cfg.Label = cfg.To
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second}
	}
	if cfg.Run == nil {
		cfg.Run = runCommand
	}
	if cfg.StatusEvery <= 0 {
		cfg.StatusEvery = statusEvery
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(cfg.Log, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
	}
	logf("listen 启动：收件方 %s（%s），执行 %s %s", cfg.To, cfg.Label, cfg.Exec, strings.Join(cfg.ExecArgs, " "))
	// 执行完指令就催一次状态上报，App 上马上看得到
	kick := make(chan struct{}, 1)
	if len(cfg.StatusArgs) > 0 {
		go reportStatus(ctx, cfg, kick, logf)
	}

	cursor := int64(-1)
	seen := map[string]int64{} // 执行过的指令 id → 过期时刻，过期后删掉
	failures := 0
	for {
		path := "/commands?label=" + url.QueryEscape(cfg.Label)
		if cursor >= 0 {
			path += "&after=" + strconv.FormatInt(cursor, 10) + "&wait=" + strconv.Itoa(listenPollWait)
		}
		var got struct {
			Commands []command.Command `json:"commands"`
			Cursor   int64             `json:"cursor"`
		}
		err := apiCall(ctx, cfg.Client, cfg.URL, cfg.Token, "GET", path, nil, &got)
		var he *httpError
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.As(err, &he) && he.status < 500:
			return fmt.Errorf("取指令失败（配置不对？）: %w", err)
		case err != nil:
			wait := cfg.Backoff[min(failures, len(cfg.Backoff)-1)]
			if failures == 0 {
				logf("连不上 pushme，%s 后重试：%v", wait, err)
			}
			failures++
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
			continue
		}
		if failures > 0 {
			logf("已重新连上 pushme")
		}
		failures = 0
		for _, c := range got.Commands {
			handleCommand(ctx, cfg, c, seen, logf)
		}
		if len(got.Commands) > 0 {
			select {
			case kick <- struct{}{}:
			default:
			}
		}
		cursor = got.Cursor
	}
}

func handleCommand(ctx context.Context, cfg listenConfig, c command.Command, seen map[string]int64, logf func(string, ...any)) {
	now := cfg.Now().Unix()
	for id, exp := range seen {
		if exp < now {
			delete(seen, id)
		}
	}
	reason := ""
	switch {
	case c.To != cfg.To:
		reason = "收件方是 " + c.To + "，不是本机"
	case approval.VerifySig(cfg.Pub, command.Payload(c), c.Sig) != nil:
		reason = "签名验不过"
	case now > c.Expires:
		reason = "已过期"
	case c.Issued > now+60:
		reason = "签发时间在未来"
	case time.Duration(c.Expires-c.Issued)*time.Second > command.MaxLife:
		reason = "有效期太长"
	}
	if _, dup := seen[c.ID]; reason == "" && dup {
		reason = "同一条指令已经执行过（重放）"
	}
	if reason != "" {
		logf("拒绝指令 id=%s by=%s：%s（%q）", c.ID, c.By, reason, c.Body)
		warn, _ := json.Marshal(map[string]string{
			"title": "⚠ " + cfg.Label + " 拒绝了一条手机指令",
			"text":  "原因：" + reason + "\n指令：" + c.Body + "\nid：" + c.ID,
		})
		if err := apiCall(ctx, cfg.Client, cfg.URL, cfg.Token, "POST", "/send", warn, &struct{}{}); err != nil {
			logf("推送警告失败：%v", err)
		}
		return
	}
	seen[c.ID] = c.Expires
	args := append(append([]string{}, cfg.ExecArgs...), strings.Fields(c.Body)...)
	env := []string{"PUSHME_CMD_ID=" + c.ID, "PUSHME_CMD_BY=" + c.By}
	logf("执行指令 id=%s by=%s：%s", c.ID, c.By, c.Body)
	out, err := cfg.Run(ctx, cfg.Exec, args, env)
	if err != nil {
		logf("指令 id=%s 执行失败：%v\n%s", c.ID, err, strings.TrimSpace(out))
		return
	}
	logf("指令 id=%s 完成：%s", c.ID, strings.TrimSpace(out))
}

// reportStatus 每 StatusEvery（或被 kick 催）跑一次状态命令，把输出报给 pushme。
// 出错只在「好 → 坏」时记一行日志，免得 pushme 没升级或断网时每 30 秒刷一次屏。
func reportStatus(ctx context.Context, cfg listenConfig, kick <-chan struct{}, logf func(string, ...any)) {
	t := time.NewTicker(cfg.StatusEvery)
	defer t.Stop()
	failing := false
	for {
		out, err := cfg.Run(ctx, cfg.Exec, cfg.StatusArgs, nil)
		status := []byte(strings.TrimSpace(out))
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			err = fmt.Errorf("状态命令出错：%w（%s）", err, status)
		case !json.Valid(status) || len(status) == 0 || status[0] != '{':
			err = fmt.Errorf("状态命令的输出不是 JSON 对象：%.200s", status)
		default:
			err = apiCall(ctx, cfg.Client, cfg.URL, cfg.Token, "POST", "/commands/status", status, &struct{}{})
		}
		if err != nil && !failing && ctx.Err() == nil {
			logf("上报状态失败：%v", err)
		} else if err == nil && failing {
			logf("上报状态恢复")
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

func runCommand(ctx context.Context, name string, args, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
