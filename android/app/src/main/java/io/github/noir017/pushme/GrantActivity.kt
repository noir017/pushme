package io.github.noir017.pushme

import android.app.Activity
import android.app.AlertDialog
import android.content.Context
import android.content.Intent
import android.hardware.biometrics.BiometricManager.Authenticators
import android.hardware.biometrics.BiometricPrompt
import android.os.Bundle
import android.os.CancellationSignal
import android.os.Handler
import android.os.Looper
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.util.Base64
import android.widget.LinearLayout
import android.widget.TextView
import java.security.SecureRandom
import java.util.concurrent.Executors

/**
 * 主动授权：不等 agent 申请，直接给某个 agent 开提权（或全部收回），相当于在终端上执行 grant。
 * 选好对象、机器、时长后过系统验证框（指纹或锁屏 PIN），用本机密钥签名，经 pushme 指令信箱发给宿主上的 pushme listen，
 * 它验签后执行 agent-grant。指令 2 分钟内有效，签名绑定了收件方、机器和时长。结果看推送消息。
 *
 * 和审批页一样：禁止截屏，被别的 App 的悬浮窗盖住时不接受点击，不在锁屏上显示。
 */
class GrantActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var targetBox: LinearLayout
    private val targetChips = LinkedHashMap<String, TextView>()
    private val hostChips = LinkedHashMap<String, TextView>()
    private lateinit var allChip: TextView
    private val durationChips = LinkedHashMap<String, TextView>()
    private lateinit var hostNote: TextView
    private lateinit var summaryView: TextView
    private lateinit var statusView: TextView
    private lateinit var grantButton: TextView
    private lateinit var revokeButton: TextView

    private var targets: List<CommandTarget> = emptyList()
    private var target: CommandTarget? = null
    private val hosts = HashSet<String>()
    private var allHosts = false
    private var duration = DEFAULT_DURATION

    /** 服务器时间减本机时间（毫秒）：签发时刻按服务器时间算，手机时钟不准也不会被当成过期。 */
    private var offsetMs = 0L
    private var busy = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        ui = Ui(this)
        targetBox = ui.column()
        for (h in HOSTS) hostChips[h.name] = ui.chip(h.label) {
            if (!hosts.remove(h.name)) hosts.add(h.name)
            allHosts = false
            render()
        }
        allChip = ui.chip("全部") {
            allHosts = !allHosts
            if (allHosts) hosts.clear()
            render()
        }
        for ((d, label) in DURATIONS) durationChips[d] = ui.chip(label) {
            duration = d
            render()
        }
        hostNote = ui.label(size = 13f, color = ui.muted)
        summaryView = ui.label(size = 14f, color = ui.muted)
        statusView = ui.notice()
        grantButton = ui.button("验证并授权") { grant() }
        revokeButton = ui.button("收回全部提权", Btn.OUTLINE, ui.bad) { confirmRevoke() }

        val body = ui.column(
            ui.header("给谁"),
            ui.card(targetBox),
            ui.header("哪些机器"),
            ui.card(ui.grid(hostChips.values.toList() + allChip, 3), hostNote, gap = 12),
            ui.header("多久"),
            ui.card(ui.grid(durationChips.values.toList(), 3)),
            ui.header("收回"),
            ui.card(ui.label("立刻收回所选 agent 在所有机器上的提权。", 14f, ui.muted), revokeButton, gap = 12),
        )
        ui.screen("主动授权", body, bottom = ui.bottomBar(statusView, summaryView, grantButton), secure = true)
        render()
        loadTargets()
    }

    override fun onDestroy() {
        io.shutdownNow()
        super.onDestroy()
    }

    private fun loadTargets() {
        if (!prefs.configured) {
            renderTargets("还没配置 pushme：回主页点「设置」填地址和接收方 token", retry = false)
            return
        }
        renderTargets("加载在线的 agent…", retry = false)
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.targets() }
            main.post {
                if (isFinishing) return@post
                result.onSuccess { t ->
                    offsetMs = if (t.now > 0) t.now * 1000 - System.currentTimeMillis() else 0
                    targets = t.targets
                    // 保留已选的；没选过就选主页点进来的那个，只有一个就选它
                    val want = target?.name ?: intent.getStringExtra(EXTRA_TARGET)
                    target = t.targets.find { it.name == want } ?: t.targets.singleOrNull()
                    renderTargets(if (t.targets.isEmpty()) "没有在线的 agent：宿主上的 pushme listen 没在跑" else null, retry = true)
                }.onFailure { renderTargets("取在线名单失败：${PushmeClient.describe(it)}", retry = true) }
                render()
            }
        }
    }

    /** 「给谁」：在线的 agent 一个一块；没有就显示 [note]（[retry] 时带「重新加载」）。 */
    private fun renderTargets(note: String?, retry: Boolean) {
        targetBox.removeAllViews()
        targetChips.clear()
        if (note != null) {
            ui.add(targetBox, ui.label(note, 14f, ui.muted))
            if (retry) ui.add(targetBox, ui.button("重新加载", Btn.TONAL) { loadTargets() }, top = 12)
            return
        }
        for (x in targets) targetChips[x.name] = ui.chip(x.label) {
            target = x
            render()
        }
        ui.add(targetBox, ui.grid(targetChips.values.toList(), 2))
    }

    /** 按当前选择刷新各块的选中状态、机器提示、摘要和按钮。 */
    private fun render() {
        for ((name, chip) in targetChips) chip.isSelected = name == target?.name
        for ((name, chip) in hostChips) chip.isSelected = name in hosts
        allChip.isSelected = allHosts
        for ((d, chip) in durationChips) chip.isSelected = d == duration

        val warnings = if (allHosts) listOf(ALL_NOTE) else HOSTS.filter { it.name in hosts }.mapNotNull { it.warning }
        hostNote.text = warnings.ifEmpty { listOf(OTHERS_NOTE) }.joinToString("\n")
        hostNote.setTextColor(if (warnings.isEmpty()) ui.muted else ui.warn)

        val t = target
        val ready = t != null && (allHosts || hosts.isNotEmpty())
        summaryView.text = when {
            t == null -> "先选给谁"
            !ready -> "再选机器"
            else -> "授权 ${t.label}：${hostsText()} ${durationText()}"
        }
        summaryView.setTextColor(if (ready) ui.ink else ui.muted)
        grantButton.enable(!busy && ready)
        revokeButton.enable(!busy && t != null)
    }

    private fun hostsText() = if (allHosts) "全部机器" else HOSTS.filter { it.name in hosts }.joinToString("、") { it.label }

    private fun durationText() = DURATIONS.first { it.first == duration }.second

    private fun grant() {
        val t = target ?: return
        val arg = if (allHosts) "all" else HOSTS.filter { it.name in hosts }.joinToString(",") { it.name }
        if (arg.isEmpty()) return
        sign(t, "grant $arg $duration", "授权 ${t.label}：${hostsText()} ${durationText()}")
    }

    private fun confirmRevoke() {
        val t = target ?: return
        AlertDialog.Builder(this)
            .setTitle("收回 ${t.label} 的全部提权？")
            .setMessage("所有机器的授权立刻收回（已复制走的证书最多再活 30 分钟）。")
            .setPositiveButton("收回") { _, _ -> sign(t, "revoke", "收回 ${t.label} 的全部提权") }
            .setNegativeButton("取消", null)
            .show()
    }

    /** 过系统验证框后签名并投递。 */
    private fun sign(target: CommandTarget, body: String, promptTitle: String) {
        if (busy) return
        val signer = try {
            Approver.signer()
        } catch (e: KeyPermanentlyInvalidatedException) {
            return ui.say(statusView, Tone.BAD, "本机密钥已被系统作废：到设置里重新生成密钥，再重新配对")
        } catch (e: Exception) {
            return ui.say(statusView, Tone.BAD, "用不了本机密钥：${e.message ?: e.javaClass.simpleName}")
        }
        val issued = (System.currentTimeMillis() + offsetMs) / 1000
        val idBytes = ByteArray(16).also { SecureRandom().nextBytes(it) }
        val cmd = SignedCommand(
            to = target.name,
            id = idBytes.joinToString("") { "%02x".format(it) },
            issued = issued,
            expires = issued + LIFE_SECONDS,
            body = body,
        )
        BiometricPrompt.Builder(this)
            .setTitle(promptTitle)
            .setSubtitle("发给 ${target.label}")
            .setDescription(body)
            .setAllowedAuthenticators(Authenticators.BIOMETRIC_STRONG or Authenticators.DEVICE_CREDENTIAL)
            .build()
            .authenticate(
                BiometricPrompt.CryptoObject(signer), CancellationSignal(), mainExecutor,
                object : BiometricPrompt.AuthenticationCallback() {
                    override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                        val sig = try {
                            val s = result.cryptoObject?.signature ?: throw IllegalStateException("没拿到签名器")
                            s.update(Approver.commandPayload(cmd))
                            Base64.encodeToString(s.sign(), Base64.NO_WRAP)
                        } catch (e: Exception) {
                            return ui.say(statusView, Tone.BAD, "签名失败：${e.message ?: e.javaClass.simpleName}")
                        }
                        send(cmd.copy(sig = sig), target)
                    }

                    override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                        ui.say(statusView, Tone.WARN, "没有发出：$errString")
                    }
                },
            )
    }

    private fun send(cmd: SignedCommand, target: CommandTarget) {
        busy = true
        render()
        ui.say(statusView, Tone.INFO, "发送中…")
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.sendCommand(cmd) }
            main.post {
                busy = false
                render()
                result.onSuccess {
                    ui.say(statusView, Tone.GOOD, "已发给 ${target.label}：${cmd.body}\n宿主几秒内执行，结果看推送消息（「agent 获得提权…」/「已手动收回」）。")
                }.onFailure { ui.say(statusView, Tone.BAD, "发送失败：${PushmeClient.describe(it)}") }
            }
        }
    }

    /** 能单台授权的机器（宿主上建了 grant-<节点名> 角色的）：节点名、显示名、选它时的提醒。 */
    private class Host(val name: String, val label: String, val warning: String?)

    companion object {
        private const val EXTRA_TARGET = "target"

        /** 打开主动授权页，预先选好 [target]（调用方名，可空）。 */
        fun intent(context: Context, target: String?): Intent =
            Intent(context, GrantActivity::class.java).apply { if (target != null) putExtra(EXTRA_TARGET, target) }

        // 指令从签发到过期：宿主 listen 只认 5 分钟以内的，2 分钟足够送到
        private const val LIFE_SECONDS = 120L
        private const val DEFAULT_DURATION = "1h"
        private val DURATIONS = listOf(
            "30m" to "30 分钟", "1h" to "1 小时", "2h" to "2 小时",
            "4h" to "4 小时", "8h" to "8 小时", "12h" to "12 小时",
        )

        private val HOSTS = listOf(
            Host("openwrt", "openwrt", "openwrt 是认证服务器，给它等于全部放开"),
            Host("unraid", "unraid", null),
            Host("oracle", "oracle", "oracle 宿主能自己续期"),
            Host("laptop", "laptop", "laptop 上有你的证书，给它等于全部放开"),
            Host("termux-mi14", "mi14", null),
        )
        /** 机器名的显示名：all 是「全部机器」，termux-mi14 是 mi14。 */
        fun hostLabel(name: String) = if (name == "all") "全部机器" else HOSTS.find { it.name == name }?.label ?: name

        private const val ALL_NOTE = "全部机器：包括 pacvue、edge 设备等不能单选的，等于全部放开"
        private const val OTHERS_NOTE = "别的机器（pacvue、edge 设备等）只能选「全部」。"
    }
}
