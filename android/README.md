# pushme 手机 App

pushme 的手机端：收消息（`GET /inbox`）、审批（`GET /approvals`、`POST /approvals/{id}`）、主动下指令（`POST /commands`）。协议见仓库根目录 [README](../README.md)「审批（手机确认）」。

- 包名 `io.github.noir017.pushme`，Android 11 起（审批密钥要用到「每次签名都验证」）。
- 不依赖 AndroidX，全部用系统 API；界面在代码里搭（配色和控件在 `Ui.kt`，颜色在 `res/values(-night)/colors.xml`，跟随系统深浅色）。

## 文件

| 文件 | 职责 |
|---|---|
| `PushService.kt` | 前台服务：长轮询收件箱弹通知；遇到待审批，按 `GET /approvals` 弹审批通知（锁屏上不显示内容） |
| `ApprovalActivity.kt` | 审批页：显示内容，批准时过系统验证框（指纹或锁屏 PIN）后签名提交；拒绝不签名。禁截屏、被悬浮窗盖住时不接受点击、不在锁屏上显示 |
| `Approver.kt` | 本机密钥（系统密钥库 EC P-256，有安全芯片就放安全芯片）、公钥导出与指纹、**签名内容**（审批、指令分别与 Go 的 `approval.Payload`、`command.Payload` 逐字节一致，改了两边一起改） |
| `Api.kt` | pushme 接收方客户端 |
| `GrantActivity.kt` | 主动授权：选在线的 agent（`GET /commands/targets`）、机器（对象上报了就用它报的清单）、时长，验证后签名投进指令信箱（`POST /commands`），那边的 `pushme listen` 验签执行；也能一键收回。也是批 agent 提权申请的页面（`GET/POST /commands/requests`）：按申请预先选好，批准 = 照选好的签一条授权指令 |
| `MainActivity.kt` | 主页：连接状态、待审批、各 agent 当前的授权（宿主 `pushme listen -status-arg _state` 上报，经 `GET /commands/targets` 取），点卡片去主动授权；在前台时有待审批就自动弹出审批页 |
| `MessagesActivity.kt` | 消息页：最近 50 条消息（`GET /inbox?after=0`，不在手机上另存）；消息通知点开到这里。待审批的正文只在审批页显示 |
| `SettingsActivity.kt` | 设置：地址和接收方 token、生成密钥、复制 / 分享公钥、通知 / 耗电 / 自启动 |
| `Ui.kt` | 配色、页面骨架（顶栏、正文、贴底按钮、系统栏留边）和常用控件；审批类页面的禁截屏、防悬浮窗也在这里统一加 |
| `BootReceiver.kt` | 开机、升级后拉起服务 |

## 密钥的规矩

- 每次签名都要当场验证（指纹或锁屏 PIN），且只有手机解锁时才能用。
- 新录入指纹不作废密钥；删掉锁屏密码会被系统作废，要重新生成、重新配对。
- 配对：设置页复制公钥，**自己**装到发起审批的机器上，用 `pushme fingerprint <公钥文件>` 打印指纹，和 App 上显示的核对。

## 构建与签名

CI（`.github/workflows/android.yml`）只出**未签名**的包，作为 Actions 产物（`pushme-0.1.<序号>-unsigned`，带 `SHA256SUMS`）。
签名密钥不进 CI，在你自己的机器上用 [`sign.ps1`](sign.ps1) 签（脚本目录里放 `apksigner.jar`、`pushme-release.jks`、`pushme-release.pass`）：

```powershell
# 第一次：生成签名密钥（只做一次；丢了就只能卸载重装、重新配对）。密码随机生成写进 .pass，不用手输
keytool -genkeypair -keystore pushme-release.jks -storetype PKCS12 -storepass:file pushme-release.pass `
        -keypass:file pushme-release.pass -alias pushme -keyalg EC -keysize 256 -sigalg SHA256withECDSA `
        -validity 36500 -dname "CN=pushme, O=noir017"

# 每次发版：先核对未签名包的 sha256 与 CI 的 SHA256SUMS 一致，再签
.\sign.ps1 .\pushme-0.1.N-unsigned.apk     # 输出 pushme-0.1.N.apk，打印证书摘要和包的 sha256
```

`apksigner.jar` 取自 Android SDK build-tools（`lib/apksigner.jar`，Java 8 起能跑），不用装整个 SDK。签名证书一旦定了就不能换，换了只能卸载重装。

**App 不自动升级**：每次都是人手动装。没让人改过 App 却冒出一个要装的新版本，别装。

## 本地检查

aarch64 上跑不了 Gradle 的安卓构建（aapt2 只有 x86_64），可以只做类型检查：JDK 17 + kotlinc 2.1 + `android-35/android.jar`，
外加一个桩 `R` 对象（代码里用到的 `R.drawable.*`、`R.string.app_name`、`R.color.*`），`kotlinc -cp android.jar` 编过即可。完整构建交给 CI。
