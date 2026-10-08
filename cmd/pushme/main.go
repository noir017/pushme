// pushme：个人消息推送小服务。收 HTTP 请求，用飞书应用机器人发给一个固定收件人；
// 配了接收方时，另把每条消息存进收件箱，供手机 App 长轮询取走。
//
// 配置全走环境变量（部署时放 .env，600）：
//
//	FEISHU_APP_ID / FEISHU_APP_SECRET  飞书应用凭据（必填）
//	PUSHME_TO                          收件人 user:ou_xxx / chat:oc_xxx / email:xxx（必填）
//	PUSHME_TOKENS                      调用方清单 name:token,name:token（必填，token 至少 16 位）
//	PUSHME_RECEIVERS                   收件箱的接收方清单 name:token,…（可选，留空不开收件箱）
//	PUSHME_LISTEN                      监听地址，默认 :8080
//	PUSHME_DATA                        数据目录（放重试队列与收件箱），默认 /data
//	PUSHME_RATE                        每个调用方的限流，默认 30/10m
//	FEISHU_BASE_URL                    默认 https://open.feishu.cn（测试时指向假服务）
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/noir017/pushme/internal/feishu"
	"github.com/noir017/pushme/internal/inbox"
	"github.com/noir017/pushme/internal/server"
	"github.com/noir017/pushme/internal/spool"
)

// version 由构建时 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	// distroless 镜像里没有 curl：compose 的 healthcheck 用 `/pushme healthcheck`。
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func healthcheck() int {
	_, port, err := net.SplitHostPort(env("PUSHME_LISTEN", ":8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "PUSHME_LISTEN:", err)
		return 1
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz:", res.Status)
		return 1
	}
	return 0
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// parseTokens 解析 "name:token,name:token"，返回 token → name；空串返回空表。
func parseTokens(s string) (map[string]string, error) {
	out := map[string]string{}
	names := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, tok, ok := strings.Cut(item, ":")
		name, tok = strings.TrimSpace(name), strings.TrimSpace(tok)
		switch {
		case !ok || name == "" || tok == "":
			return nil, fmt.Errorf("条目应为 name:token，%q 不是", name)
		case len(tok) < 16:
			return nil, fmt.Errorf("%s 的 token 太短（至少 16 位）", name)
		case names[name]:
			return nil, fmt.Errorf("名字 %s 重复", name)
		case out[tok] != "":
			return nil, fmt.Errorf("%s 与 %s 用了同一个 token", out[tok], name)
		}
		names[name] = true
		out[tok] = name
	}
	return out, nil
}

// parseReceivers 解析收件箱的接收方，token 不能和调用方的重复（否则分不清是发还是收）。
func parseReceivers(s string, callers map[string]string) (map[string]string, error) {
	out, err := parseTokens(s)
	if err != nil {
		return nil, err
	}
	for tok, name := range out {
		if c := callers[tok]; c != "" {
			return nil, fmt.Errorf("接收方 %s 与调用方 %s 用了同一个 token", name, c)
		}
	}
	return out, nil
}

func names(tokens map[string]string) string {
	out := make([]string, 0, len(tokens))
	for _, n := range tokens {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func run(log *slog.Logger) error {
	appID, appSecret := env("FEISHU_APP_ID", ""), env("FEISHU_APP_SECRET", "")
	if appID == "" || appSecret == "" {
		return errors.New("FEISHU_APP_ID / FEISHU_APP_SECRET 未设置")
	}
	to, err := feishu.ParseReceiver(env("PUSHME_TO", ""))
	if err != nil {
		return fmt.Errorf("PUSHME_TO: %w", err)
	}
	tokens, err := parseTokens(env("PUSHME_TOKENS", ""))
	if err != nil {
		return fmt.Errorf("PUSHME_TOKENS: %w", err)
	}
	if len(tokens) == 0 {
		return errors.New("PUSHME_TOKENS 为空")
	}
	receivers, err := parseReceivers(env("PUSHME_RECEIVERS", ""), tokens)
	if err != nil {
		return fmt.Errorf("PUSHME_RECEIVERS: %w", err)
	}
	burst, window, err := server.ParseRate(env("PUSHME_RATE", "30/10m"))
	if err != nil {
		return fmt.Errorf("PUSHME_RATE: %w", err)
	}
	dataDir := env("PUSHME_DATA", "/data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	fc := &feishu.Client{BaseURL: env("FEISHU_BASE_URL", feishu.DefaultBaseURL), AppID: appID, AppSecret: appSecret, To: to}
	sp, err := spool.Open(filepath.Join(dataDir, "spool.json"),
		func(ctx context.Context, e spool.Entry) (string, error) { return fc.SendText(ctx, e.Text) },
		feishu.IsTemporary, nil, log)
	if err != nil {
		return err
	}
	var box *inbox.Inbox
	if len(receivers) > 0 {
		if box, err = inbox.Open(filepath.Join(dataDir, "inbox.json"), nil); err != nil {
			return err
		}
	}

	srv := &server.Server{
		Tokens:    tokens,
		Receivers: receivers,
		Sender:    fc,
		Queue:     sp,
		Inbox:     box,
		Temporary: feishu.IsTemporary,
		Limiter:   &server.Limiter{Burst: burst, Window: window},
		Log:       log,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{
		Addr:              env("PUSHME_LISTEN", ":8080"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// 收到退出信号时请求的 ctx 随之结束：挂着的收件箱长轮询立即返回，不拖住优雅退出
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	log.Info("pushme starting", "version", version, "listen", httpSrv.Addr, "to", to.Kind(),
		"callers", names(tokens), "receivers", names(receivers),
		"rate", fmt.Sprintf("%d/%s", burst, window), "queued", sp.Len())

	go sp.Run(ctx, 15*time.Second)

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
