# pushme

给自己推消息的小服务：各处的脚本、定时任务、服务通过 HTTP 把告警或通知交给它，它用**飞书应用机器人**发到一个固定的收件人（私聊或群）。

- 飞书凭据只放在 pushme 一处，调用方只拿各自的 token。
- **兼容飞书自定义机器人的 webhook 格式**：本来就支持「飞书 webhook」的工具（acme.sh 的 `feishu` 通知钩子、很多监控系统）只要把 URL 换成 pushme 的，不用改代码。
- 每个调用方一个 token：消息自动加 `[调用方]` 前缀，可单独吊销，单独限流。
- 飞书暂时发不出去时，消息写入本地队列并按退避重试，最多重试 24 小时，调用方不必自己处理重试。
- Go 标准库，零依赖；镜像约 10MB（distroless），内存 < 20MB；amd64 / arm64。

## 接口

### `POST /send`：原生接口

```sh
# 纯文本
curl -m 10 -s -H "Authorization: Bearer $PUSHME_TOKEN" \
     --data-binary "磁盘快满了：/mnt/cache 93%" http://<host>:8290/send

# JSON，带标题
curl -m 10 -s -H "Authorization: Bearer $PUSHME_TOKEN" -H 'Content-Type: application/json' \
     -d '{"title":"备份失败","text":"gsyncer oracle-apps exit 23"}' http://<host>:8290/send
```

返回 `{"ok":true,"message_id":"om_…","queued":false}`。`queued:true` 表示飞书暂时发不出去，已经进了重试队列。

### `POST /hook/{token}`：飞书自定义机器人兼容接口

请求体与飞书自定义机器人一致，支持 `text` 与 `post`（富文本会展平成纯文本）：

```json
{"msg_type":"text","content":{"text":"Renew success\nexample.com"}}
```

响应与飞书一致：`{"code":0,"msg":"success","StatusCode":0,"StatusMessage":"success","data":{…}}`。
请求里带的 `timestamp`/`sign` 会被忽略：URL 里的 token 本身就是凭据。

### `GET /healthz`

返回 `{"ok":true,"queued":<队列长度>}`。

### 状态码

| 状态 | 含义 |
|---|---|
| 200 | 已发出，或已进入重试队列（看 `queued`） |
| 400 | 请求体不对（空消息、不支持的 `msg_type`、坏 JSON） |
| 401 | token 不对 |
| 429 | 该调用方超过限流；每个窗口第一次被限流时会额外推一条提醒 |
| 502 | 飞书明确拒绝（收件人不对、应用没权限等），不会重试 |

## 调用示例

**acme.sh**：证书续期结果通知，用它自带的 `feishu` 钩子，不改代码。

```sh
export FEISHU_WEBHOOK="http://<host>:8290/hook/<acme 的 token>"
acme.sh --set-notify --notify-hook feishu --notify-level 2   # 2 = 失败和成功都通知；会立刻发一条测试
```

**支持飞书 webhook 的程序**：把 webhook URL 设成 `http://<host>:8290/hook/<token>` 即可。

**BusyBox / OpenWrt 脚本**：只需要 `curl`。

```sh
[ -r /path/pushme.env ] && . /path/pushme.env   # 里面放 PUSHME_URL / PUSHME_TOKEN
curl -m 10 -s -H "Authorization: Bearer $PUSHME_TOKEN" --data-binary "$title
$detail" "$PUSHME_URL/send" >/dev/null 2>&1
```

## 部署

```sh
mkdir -p /srv/pushme && cd /srv/pushme
curl -fsSLO https://raw.githubusercontent.com/noir017/pushme/main/deploy/docker-compose.yml
curl -fsSL  https://raw.githubusercontent.com/noir017/pushme/main/deploy/.env.example -o .env
chmod 600 .env && $EDITOR .env
mkdir -p data && sudo chown 65532:65532 data   # 镜像以 nonroot(65532) 运行
docker compose up -d && docker compose logs -f
```

启动日志只打印调用方名、收件人类型和限流配置，不会打出凭据。

### 配置（环境变量）

| 变量 | 必填 | 说明 |
|---|---|---|
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | ✓ | 飞书开放平台自建应用的凭据，应用需开通机器人能力和发消息权限 |
| `PUSHME_TO` | ✓ | 收件人：`user:ou_xxx` / `chat:oc_xxx` / `email:xxx`。调用方不能指定收件人 |
| `PUSHME_TOKENS` | ✓ | `name:token,name:token`；token 至少 16 位，建议 `openssl rand -hex 24` |
| `PUSHME_RATE` | | 每个调用方的限流，默认 `30/10m` |
| `PUSHME_LISTEN` | | 容器内监听地址，默认 `:8080` |
| `PUSHME_DATA` | | 重试队列所在目录，默认 `/data` |
| `PUSHME_BIND` / `PUSHME_PORT` | | 只给 compose 插值用：宿主机绑定的 IP 与端口 |

新增调用方：往 `PUSHME_TOKENS` 加一项，然后 `docker compose up -d`。吊销调用方：删掉那一项，同样重启。

### 更新

push 到 `main`，CI 测试通过后会发布 `ghcr.io/noir017/pushme:latest`（以及 `sha-<短 sha>`）。部署机上执行：

```sh
docker compose pull && docker compose up -d
```

## 安全

- 只打算在内网用：compose 模板默认绑 `127.0.0.1`，应改成部署机的内网 IP，**别绑 0.0.0.0 暴露到公网**。
- token 放在 `/hook/{token}` 的 URL 里（为了兼容飞书 webhook 格式），会出现在调用方的配置里。要是经过会记录 URL 的反向代理，请关掉这一路径的 access log。
- 日志只记调用方、字节数、结果和飞书 message_id，不记消息正文。

## 开发

```sh
go test -race ./...
```

飞书接口在测试里用 `httptest` 模拟。本地手动试时，把 `FEISHU_BASE_URL` 指向一个假的服务。
