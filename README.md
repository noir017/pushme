# pushme

给自己推消息的小服务：各处的脚本、定时任务、服务通过 HTTP 把告警或通知交给它，它用**飞书应用机器人**发到一个固定的收件人（私聊或群）。

- 飞书凭据只放在 pushme 一处，调用方只拿各自的 token。
- **兼容飞书自定义机器人的 webhook 格式**：本来就支持「飞书 webhook」的工具（acme.sh 的 `feishu` 通知钩子、很多监控系统）只要把 URL 换成 pushme 的，不用改代码。
- 每个调用方一个 token：消息自动加 `[调用方]` 前缀，可单独吊销，单独限流。
- 飞书暂时发不出去时，消息写入本地队列并按退避重试，最多重试 24 小时，调用方不必自己处理重试。
- 可选的**收件箱**：每条消息另存一份，手机 App 长轮询 `GET /inbox` 收、直接弹通知（例如 AutoAnything 的「aa助手」），不经飞书。
- 可选的**审批（手机确认）**：脚本发起一条审批，手机上看完内容、用本机密钥签名批准，脚本用钉死的手机公钥验签（`pushme ask`）。反过来手机也能主动下签好名的指令（`pushme listen`）。pushme 只转发，伪造不了。
- Go 标准库，零依赖；镜像约 10MB（distroless），内存 < 20MB；amd64 / arm64。

## 接口

### `POST /send`：原生接口

```sh
# 纯文本
curl -m 10 -s -H "Authorization: Bearer $PUSHME_TOKEN" \
     --data-binary "磁盘快满了：/mnt/cache 93%" http://<host>:8290/send

# JSON，带标题
curl -m 10 -s -H "Authorization: Bearer $PUSHME_TOKEN" -H 'Content-Type: application/json' \
     -d '{"title":"备份失败","text":"gsyncer cloud-apps exit 23"}' http://<host>:8290/send
```

返回 `{"ok":true,"message_id":"om_…","queued":false}`。`queued:true` 表示飞书暂时发不出去，已经进了重试队列。

### `POST /hook/{token}`：飞书自定义机器人兼容接口

请求体与飞书自定义机器人一致，支持 `text` 与 `post`（富文本会展平成纯文本）：

```json
{"msg_type":"text","content":{"text":"Renew success\nexample.com"}}
```

响应与飞书一致：`{"code":0,"msg":"success","StatusCode":0,"StatusMessage":"success","data":{…}}`。
请求里带的 `timestamp`/`sign` 会被忽略：URL 里的 token 本身就是凭据。

### `GET /inbox`：收件箱（手机 App 收消息）

配了 `PUSHME_RECEIVERS` 才有（否则 404）。每条被受理的消息（被限流的除外）按到达顺序编号存一份，只留最近 200 条；
与飞书发没发成功无关。鉴权用**接收方** token，调用方的 token 读不了。

```sh
curl -m 70 -s -H "Authorization: Bearer $RECEIVER_TOKEN" "http://<host>:8290/inbox?after=41&wait=50&limit=20"
```

```json
{"ok":true,"messages":[{"seq":42,"ts":1791234567,"caller":"openwrt","title":"备份失败","text":"exit 23"}],"skipped":0,"cursor":42}
```

- 返回序号大于 `after` 的消息里**最新的** `limit` 条（默认 20，最多 100），按序号升序；`skipped` 是因此没返回的较早条数（含已被丢弃的）。
- 没有新消息时最多挂 `wait` 秒（最多 60），一来新消息立即返回。
- 下次请求把 `after` 设成响应里的 `cursor`。不带 `after`（或为负）时不回放历史，立即返回当前 `cursor`，第一次连上时用。
- `after` 比 `cursor` 还大说明收件箱被清空过，这时从头返回现存的消息。
- `title` / `text` 分开给，不带 `[调用方]` 前缀（调用方在 `caller` 里）。
- 待审批的消息多一个 `approval: {"id", "expires"}` 字段（见下节）；不认识它的 App 当普通消息显示。

### 审批（手机确认）

和收件箱一起开（配了 `PUSHME_RECEIVERS` 才有，否则 404）。用途：宿主上的脚本要做敏感操作前（如给 agent 提权），让人在手机上确认。

```
脚本 ── pushme ask ── POST /approvals ──▶ pushme ──▶ 收件箱（带 approval）+ 飞书提醒一句
手机 App ── GET /approvals ──▶ 看内容 → 指纹 / PIN → 本机密钥签名 ── POST /approvals/{id} ──▶ pushme
pushme ask ── GET /approvals/{id}?wait= ──▶ 拿到签名，用钉死的手机公钥验签 → 退出码
```

| 接口 | 鉴权 | 说明 |
|---|---|---|
| `POST /approvals` | 调用方 | `{"title","text","ttl":秒}` → `{"id","challenge","expires"}`。ttl 默认 300，截到 30～3600。标题 ≤200 字节、正文 ≤4000 字节，超了 400（不截断：内容原样参与签名）。和 `/send` 共用限流；每个调用方同时最多挂 3 条，超了 429 |
| `GET /approvals/{id}?wait=秒` | 发起它的调用方 | `{"state":"pending｜approved｜denied｜expired","sig","by","answered","expires"}`。pending 时最多挂 `wait` 秒（≤60），一有答复立即返回。别的调用方取不到（404）。终态保留 10 分钟 |
| `GET /approvals` | 接收方 | 还在等答复的：`{"approvals":[{"id","caller","title","text","challenge","created","expires","state"}],"now":服务器时间}` |
| `POST /approvals/{id}` | 接收方 | `{"decision":"approve","sig":"<base64>"}` 或 `{"decision":"deny"}`。只认第一次（之后 409）；过期了 409 |

**签名约定**（App 与 `pushme ask` 的契约，改了两边都得改）：手机密钥是 EC P-256，算法 `SHA256withECDSA`，`sig` 是 ASN.1 DER 签名的 base64。签名内容是下面 7 行，`\n` 分隔、末尾不带换行：

```
pushme-approval-v1
<id>
<challenge>
approve
<expires>
<sha256hex(title)>
<sha256hex(text)>
```

pushme 不验签。验签的是发起方：它用**自己发出去的** title/text 和创建时拿到的 id/challenge/expires 拼内容，所以 pushme 改了显示给手机的内容、或者有人拿到接收方 token 自己答复，都验不过。拒绝不签名，发起方把「没验过的批准」一律当拒绝。

**`pushme ask`**：同一个二进制的子命令，给宿主上的脚本用。

```sh
PUSHME_URL=http://<host>:8290 PUSHME_TOKEN=<调用方 token> \
  pushme ask -pubkey /path/approver.pub -title "uniagent 申请提权" -text "unraid（root）1 小时" [-ttl 5m]
```

stdout 一行结果：`approved` / `denied` / `expired` / `bad-signature`。退出码 0 = 批准且签名有效，1 = 拒绝、过期或签名不对，2 = 用法、配置或网络出错。
token 只从环境变量读（不出现在进程参数里）。等待中连不上 pushme 会退避重试，直到过期。

**配对**：手机公钥是 PEM（`-----BEGIN PUBLIC KEY-----`，SubjectPublicKeyInfo）。装到宿主后用 `pushme fingerprint approver.pub` 打印指纹（SubjectPublicKeyInfo DER 的 SHA-256 前 8 字节，如 `ab12 cd34 ef56 7890`），和 App 上显示的核对。**这一步要人自己做**：谁替你传公钥，谁就能换成他的。

**手机 App** 在 [`android/`](android/README.md)：收消息、审批。CI 只出未签名的包，签名在你自己的机器上做。

### 指令信箱（手机主动下指令）

和审批反过来：手机主动给某个调用方下一条签好名的指令（例如「给 uniagent 开 unraid 1 小时提权」），调用方在宿主上跑 `pushme listen` 取走、验签、执行。
同样配了 `PUSHME_RECEIVERS` 才有；同一把手机密钥，pushme 不验签。

| 接口 | 鉴权 | 说明 |
|---|---|---|
| `POST /commands` | 接收方 | `{"to":"<调用方名>","id","issued","expires","body","sig"}`。`body` 一行 ≤200 字节；`expires - issued` ≤5 分钟；`to` 必须是已有调用方 |
| `GET /commands?after=&wait=&label=` | 调用方 | 取发给自己的指令（长轮询，≤60 秒）；不带 `after` 只返回当前 `cursor`。顺带登记自己在线，`label` 是在 App 上显示的名字 |
| `GET /commands/targets` | 接收方 | 3 分钟内来取过指令的调用方 `{"targets":[{"name","label","last_seen","status","status_at"}],"now"}`，App 据此列出能下指令的对象、显示它们上报的状态 |
| `POST /commands/status` | 调用方 | body 是一个 JSON 对象（≤4KB），作为本调用方的当前状态挂到在线名单上（`status`）。pushme 不解析，原样转给 App；只放内存 |

指令只放内存，几分钟就过期，pushme 重启就丢。签名内容（6 行，`\n` 分隔，末尾不带换行）：

```
pushme-command-v1
<id>
<to>
<issued>
<expires>
<body>
```

**`pushme listen`**：

```sh
PUSHME_URL=http://<host>:8290 PUSHME_TOKEN=<调用方 token> \
  pushme listen -to <本调用方名> -label <显示名> -pubkey /path/approver.pub -exec /usr/local/sbin/agent-grant -exec-arg _cmd [-status-arg _state]
```

加 `-status-arg _state` 时，每 30 秒和每执行完一条指令跑一次 `-exec _state`，把输出（一个 JSON 对象）经 `POST /commands/status` 报上去。
App 认的格式（`agent-grant _state` 输出的）：`{"grants":[{"host":"unraid","expires":<epoch 秒>,"duration":"1h","by":"…"}],"broken":["…"]}`，
`host` 为 `all` 表示全部机器，`broken` 是源证书失效、授权了也用不了的机器。

只执行收件方是 `-to`、签名对得上、没过期（签发不晚于现在 1 分钟、有效期 ≤5 分钟）、同一个 `id` 没执行过的指令；
正文按空白切开接在 `-exec` / `-exec-arg` 后面执行（不经 shell），环境变量带 `PUSHME_CMD_ID`、`PUSHME_CMD_BY`。
验不过的不执行，经 `/send` 推一条警告。一直跑，连不上 pushme 就退避重试，token 不对直接退出（交给 systemd / 看门狗重启）。

### `GET /healthz`

返回 `{"ok":true,"queued":<队列长度>}`。

### 状态码

| 状态 | 含义 |
|---|---|
| 200 | 已发出，或已进入重试队列（看 `queued`） |
| 400 | 请求体不对（空消息、不支持的 `msg_type`、坏 JSON） |
| 401 | token 不对 |
| 404 | 收件箱 / 审批没开，或审批不存在 |
| 409 | 审批已答复或已过期 |
| 429 | 该调用方超过限流（每个窗口第一次被限流时会额外推一条提醒），或挂着的审批太多 |
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
| `PUSHME_RECEIVERS` | | 收件箱的接收方，格式同上，token 不能与调用方重复；留空不开收件箱和审批 |
| `PUSHME_RATE` | | 每个调用方的限流，默认 `30/10m` |
| `PUSHME_LISTEN` | | 容器内监听地址，默认 `:8080` |
| `PUSHME_DATA` | | 重试队列、收件箱与审批所在目录，默认 `/data` |
| `PUSHME_BIND` / `PUSHME_PORT` | | 只给 compose 插值用：宿主机绑定的 IP 与端口 |

新增调用方：往 `PUSHME_TOKENS` 加一项，然后 `docker compose up -d`。吊销调用方：删掉那一项，同样重启。接收方同理（`PUSHME_RECEIVERS`）。

### 更新

push 到 `main`，CI 测试通过后会发布 `ghcr.io/noir017/pushme:latest`（以及 `sha-<短 sha>`）。部署机上执行：

```sh
docker compose pull && docker compose up -d
```

## 安全

- 只打算在内网用：compose 模板默认绑 `127.0.0.1`，应改成部署机的内网 IP，**别绑 0.0.0.0 暴露到公网**。
- token 放在 `/hook/{token}` 的 URL 里（为了兼容飞书 webhook 格式），会出现在调用方的配置里。要是经过会记录 URL 的反向代理，请关掉这一路径的 access log。
- 日志只记调用方、字节数、结果和飞书 message_id，不记消息正文。收件箱（`data/inbox.json`）和审批（`data/approvals.json`）存了正文，和 `.env` 一样别外传。
- 审批的安全不靠 pushme：靠手机里导不出来的私钥，和发起方那份钉死的公钥。`pushme ask` 所在的机器要是被人拿下，他可以绕过审批直接干活，这不归 pushme 管。

## 开发

```sh
go test -race ./...
```

飞书接口在测试里用 `httptest` 模拟。本地手动试时，把 `FEISHU_BASE_URL` 指向一个假的服务。
