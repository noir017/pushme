# pushme 手机 App

pushme 的手机端：收消息（`GET /inbox`）、审批（`GET /approvals`、`POST /approvals/{id}`）。协议见仓库根目录 [README](../README.md)「审批（手机确认）」。

- 包名 `io.github.noir017.pushme`，Android 11 起（审批密钥要用到「每次签名都验证」）。
- 不依赖 AndroidX，全部用系统 API；界面在代码里搭。

## 文件

| 文件 | 职责 |
|---|---|
| `PushService.kt` | 前台服务：长轮询收件箱弹通知；遇到待审批，按 `GET /approvals` 弹审批通知（锁屏上不显示内容） |
| `ApprovalActivity.kt` | 审批页：显示内容，批准时过系统验证框（指纹或锁屏 PIN）后签名提交；拒绝不签名。禁截屏、被悬浮窗盖住时不接受点击、不在锁屏上显示 |
| `Approver.kt` | 本机密钥（系统密钥库 EC P-256，有安全芯片就放安全芯片）、公钥导出与指纹、**签名内容**（与 Go 的 `approval.Payload` 逐字节一致，改了两边一起改） |
| `Api.kt` | pushme 接收方客户端 |
| `MainActivity.kt` | 填地址和接收方 token、生成密钥、复制 / 分享公钥、权限设置 |
| `BootReceiver.kt` | 开机、升级后拉起服务 |

## 密钥的规矩

- 每次签名都要当场验证（指纹或锁屏 PIN），且只有手机解锁时才能用。
- 新录入指纹不作废密钥；删掉锁屏密码会被系统作废，要重新生成、重新配对。
- 配对：主界面复制公钥，**自己**装到发起审批的机器上，用 `pushme fingerprint <公钥文件>` 打印指纹，和 App 上显示的核对。

## 构建与签名

CI（`.github/workflows/android.yml`）只出**未签名**的包，作为 Actions 产物（`pushme-0.1.<序号>-unsigned`，带 `SHA256SUMS`）。
签名密钥不进 CI，在你自己的机器上签：

```sh
# 第一次：生成签名密钥（只做一次，丢了就只能卸载重装、重新配对）
keytool -genkeypair -keystore pushme-release.jks -alias pushme -keyalg EC -groupname secp256r1 \
        -validity 36500 -dname "CN=pushme"

# 每次发版：签名、核对
apksigner sign --ks pushme-release.jks --ks-pass file:pushme-release.pass \
               --out pushme-0.1.N.apk pushme-0.1.N-unsigned.apk
apksigner verify --print-certs pushme-0.1.N.apk
```

`apksigner` 在 Android SDK 的 build-tools 里（也可以单独用 `java -jar apksigner.jar`）。签名证书一旦定了就不能换，换了只能卸载重装。

**App 不自动升级**：每次都是人手动装。没让人改过 App 却冒出一个要装的新版本，别装。

## 本地检查

aarch64 上跑不了 Gradle 的安卓构建（aapt2 只有 x86_64），可以只做类型检查：JDK 17 + kotlinc 2.1 + `android-35/android.jar`，
外加一个只有 `R.drawable.ic_stat`、`R.string.app_name` 的桩 `R` 对象，`kotlinc -cp android.jar` 编过即可。完整构建交给 CI。
