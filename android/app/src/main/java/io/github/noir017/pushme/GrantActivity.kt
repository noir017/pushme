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
import android.view.View
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import java.security.SecureRandom
import java.util.concurrent.Executors

/**
 * 主动授权：直接给某个 agent 开提权（或全部收回），相当于在终端上执行 grant；也用来批 agent 的提权申请。
 * 选好对象、机器、时长后过系统验证框（指纹或锁屏 PIN），用本机密钥签名，经 pushme 指令信箱发给那个 agent 的 pushme listen
 * （宿主上的 agent-grant，或 openwrt 上的 grant-broker），它验签后执行。指令 2 分钟内有效，签名绑定了收件方、机器和时长。
 *
 * 批申请（从 [requestIntent] 打开）：按申请内容预先选好，上面显示理由；批准就是照选好的签一条授权指令，再告诉 pushme「批了」好让 agent 知道。
 * 能选哪些机器：对象上报了（grant-broker 会报）就用它的，没报就用 [HOSTS]。
 *
 * 和审批页一样：禁止截屏，被别的 App 的悬浮窗盖住时不接受点击，不在锁屏上显示。
 */
class GrantActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var requestBox: LinearLayout
    private lateinit var targetBox: LinearLayout
    private lateinit var hostBox: LinearLayout
    private lateinit var durationBox: LinearLayout
    private val targetChips = LinkedHashMap<String, TextView>()
    private val hostChips = LinkedHashMap<String, TextView>()
    private var allChip: TextView? = null
    private val durationChips = LinkedHashMap<String, TextView>()
    private lateinit var hostNote: TextView
    private lateinit var summaryView: TextView
    private lateinit var statusView: TextView
    private lateinit var grantButton: TextView
    private lateinit var denyButton: TextView
    private lateinit var revokeCard: View
    private lateinit var revokeButton: TextView

    private var targets: List<CommandTarget> = emptyList()
    private var target: CommandTarget? = null
    private var hostOptions: List<HostOption> = defaultHosts()
    private val hosts = HashSet<String>()
    private var allHosts = false
    private var duration = DEFAULT_DURATION
    private var durations = DURATIONS

    /** 正在批的申请（从 [requestIntent] 打开时）；处理完或找不到就为空。 */
    private var request: GrantRequest? = null

    /** 服务器时间减本机时间（毫秒）：签发时刻按服务器时间算，手机时钟不准也不会被当成过期。 */
    private var offsetMs = 0L
    private var busy = false

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        ui = Ui(this)
        requestBox = ui.column()
        targetBox = ui.column()
        hostBox = ui.column()
        durationBox = ui.column()
        hostNote = ui.label(size = 13f, color = ui.muted)
        summaryView = ui.label(size = 14f, color = ui.muted)
        statusView = ui.notice()
        grantButton = ui.button("验证并授权") { grant() }
        denyButton = ui.button("拒绝申请", Btn.OUTLINE, ui.bad) { deny() }.apply { visibility = View.GONE }
        revokeButton = ui.button("收回全部提权", Btn.OUTLINE, ui.bad) { confirmRevoke() }
        revokeCard = ui.card(ui.label("立刻收回所选 agent 在所有机器上的提权。", 14f, ui.muted), revokeButton, gap = 12)

        val body = ui.column(
            requestBox,
            ui.header("给谁"),
            ui.card(targetBox),
            ui.header("哪些机器"),
            ui.card(hostBox, hostNote, gap = 12),
            ui.header("多久"),
            ui.card(durationBox),
            ui.header("收回"),
            revokeCard,
        )
        val buttons = ui.row(denyButton, grantButton, weights = listOf(1f, 1.6f))
        ui.screen(if (requestId() != null) "批提权申请" else "主动授权", body, bottom = ui.bottomBar(statusView, summaryView, buttons), secure = true)
        renderHosts()
        renderDurations()
        render()
        load()
    }

    override fun onDestroy() {
        io.shutdownNow()
        super.onDestroy()
    }

    private fun requestId(): String? = intent.getStringExtra(EXTRA_REQUEST)

    /** 取在线的对象（和要批的那条申请）。 */
    private fun load() {
        if (!prefs.configured) {
            renderTargets("还没配置 pushme：回主页点「设置」填地址和接收方 token", retry = false)
            return
        }
        renderTargets("加载在线的 agent…", retry = false)
        val client = PushmeClient(prefs.url, prefs.token)
        val rid = requestId()
        io.execute {
            val result = runCatching { client.targets() }
            val req = if (rid == null) null else runCatching { client.requests().requests.find { it.id == rid } }
            main.post {
                if (isFinishing) return@post
                result.onSuccess { t ->
                    offsetMs = if (t.now > 0) t.now * 1000 - System.currentTimeMillis() else 0
                    targets = t.targets
                    // 保留已选的；没选过就选申请里的、或主页点进来的那个；只有一个就选它
                    val want = target?.name ?: req?.getOrNull()?.target ?: intent.getStringExtra(EXTRA_TARGET)
                    target = t.targets.find { it.name == want } ?: t.targets.singleOrNull()
                    renderTargets(if (t.targets.isEmpty()) "没有在线的 agent：它的 pushme listen 没在跑" else null, retry = true)
                }.onFailure { renderTargets("取在线名单失败：${PushmeClient.describe(it)}", retry = true) }
                hostOptions = target?.status?.hosts?.ifEmpty { null } ?: defaultHosts()
                if (rid != null) showRequest(req?.getOrNull(), req?.exceptionOrNull())
                renderHosts()
                renderDurations()
                render()
            }
        }
    }

    /** 批申请：按申请内容预先选好，上面显示申请人和理由；找不到（处理过、过期）就当普通的主动授权。 */
    private fun showRequest(req: GrantRequest?, error: Throwable?) {
        request = req
        requestBox.removeAllViews()
        if (req == null) {
            ui.say(statusView, Tone.WARN, if (error != null) "取申请失败：${PushmeClient.describe(error)}" else "这条申请已经处理过或已过期，下面可以照常主动授权")
            denyButton.visibility = View.GONE
            grantButton.text = "验证并授权"
            return
        }
        val asked = req.hosts.split(",").map { if (it == "mi14") "termux-mi14" else it }
        val known = hostOptions.map { it.name }
        allHosts = "all" in asked || asked.any { it !in known }
        hosts.clear()
        if (!allHosts) hosts.addAll(asked)
        if (durations.none { it.first == req.duration }) durations = durations + (req.duration to req.duration)
        duration = req.duration
        val unknown = asked.filter { it != "all" && it !in known }
        val lines = mutableListOf<View>(
            ui.label("${req.caller} 申请提权：${req.hosts} ${req.duration}", 17f, bold = true),
            ui.label("理由：${req.reason}", 15f),
        )
        if (unknown.isNotEmpty()) lines += ui.label("${unknown.joinToString("、") { hostLabel(it) }} 没有单台授权，只能选「全部」", 13f, ui.warn)
        if (req.target != target?.name) lines += ui.label("申请要发给 ${req.target}，但它现在不在线", 13f, ui.bad)
        lines += ui.label("可以先改机器、时长再批；拒绝不用验证。", 13f, ui.muted)
        ui.add(requestBox, ui.card(*lines.toTypedArray(), gap = 6, stroke = ui.alpha(ui.warn, 0x99)), top = 8)
        denyButton.visibility = View.VISIBLE
        grantButton.text = "验证并批准"
        revokeCard.visibility = View.GONE
    }

    /** 「给谁」：在线的 agent 一个一块；没有就显示 [note]（[retry] 时带「重新加载」）。 */
    private fun renderTargets(note: String?, retry: Boolean) {
        targetBox.removeAllViews()
        targetChips.clear()
        if (note != null) {
            ui.add(targetBox, ui.label(note, 14f, ui.muted))
            if (retry) ui.add(targetBox, ui.button("重新加载", Btn.TONAL) { load() }, top = 12)
            return
        }
        for (x in targets) targetChips[x.name] = ui.chip(x.label) {
            target = x
            // 不同对象能选的机器可能不一样（grant-broker 报的，或旧宿主的默认清单）
            hostOptions = x.status?.hosts?.ifEmpty { null } ?: defaultHosts()
            hosts.retainAll(hostOptions.map { it.name }.toSet())
            renderHosts()
            render()
        }
        ui.add(targetBox, ui.grid(targetChips.values.toList(), 2))
    }

    private fun renderHosts() {
        hostBox.removeAllViews()
        hostChips.clear()
        for (h in hostOptions) hostChips[h.name] = ui.chip(hostLabel(h.name)) {
            if (!hosts.remove(h.name)) hosts.add(h.name)
            allHosts = false
            render()
        }
        allChip = ui.chip("全部") {
            allHosts = !allHosts
            if (allHosts) hosts.clear()
            render()
        }
        ui.add(hostBox, ui.grid(hostChips.values.toList() + allChip!!, 3))
    }

    private fun renderDurations() {
        durationBox.removeAllViews()
        durationChips.clear()
        for ((d, label) in durations) durationChips[d] = ui.chip(label) {
            duration = d
            render()
        }
        ui.add(durationBox, ui.grid(durationChips.values.toList(), 3))
    }

    /** 按当前选择刷新各块的选中状态、机器提示、摘要和按钮。 */
    private fun render() {
        for ((name, chip) in targetChips) chip.isSelected = name == target?.name
        for ((name, chip) in hostChips) chip.isSelected = name in hosts
        allChip?.isSelected = allHosts
        for ((d, chip) in durationChips) chip.isSelected = d == duration

        val warnings = if (allHosts) listOf(ALL_NOTE) else hostOptions.filter { it.name in hosts }.mapNotNull { it.note }
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
        denyButton.enable(!busy)
        revokeButton.enable(!busy && t != null)
    }

    private fun selectedHosts() = hostOptions.filter { it.name in hosts }

    private fun hostsText() = if (allHosts) "全部机器" else selectedHosts().joinToString("、") { hostLabel(it.name) }

    private fun durationText() = durations.first { it.first == duration }.second

    private fun grant() {
        val t = target ?: return
        val arg = if (allHosts) "all" else selectedHosts().joinToString(",") { it.name }
        if (arg.isEmpty()) return
        sign(t, "grant $arg $duration", "授权 ${t.label}：${hostsText()} ${durationText()}")
    }

    private fun confirmRevoke() {
        val t = target ?: return
        AlertDialog.Builder(this)
            .setTitle("收回 ${t.label} 的全部提权？")
            .setMessage("所有机器的授权立刻收回。还在用宿主 agent-grant 的对象，已复制走的证书最多再活 30 分钟。")
            .setPositiveButton("收回") { _, _ -> sign(t, "revoke", "收回 ${t.label} 的全部提权") }
            .setNegativeButton("取消", null)
            .show()
    }

    /** 拒绝申请：不签名，只告诉 pushme，agent 那边就不等了。 */
    private fun deny() {
        val req = request ?: return
        if (busy) return
        busy = true
        render()
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.answerRequest(req.id, false) }
            main.post {
                busy = false
                result.onSuccess {
                    PushService.cancelRequest(this, req.id)
                    Toast.makeText(this, "已拒绝", Toast.LENGTH_SHORT).show()
                    finish()
                }.onFailure {
                    render()
                    ui.say(statusView, Tone.BAD, "没拒绝成功：${PushmeClient.describe(it)}")
                }
            }
        }
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

    /** 投递指令；批申请时再告诉 pushme「批了」（记不上也不要紧：授权靠的是指令，agent 那边会发现证书已经生效）。 */
    private fun send(cmd: SignedCommand, target: CommandTarget) {
        busy = true
        render()
        ui.say(statusView, Tone.INFO, "发送中…")
        val client = PushmeClient(prefs.url, prefs.token)
        val req = request?.takeIf { cmd.body.startsWith("grant ") }
        io.execute {
            val result = runCatching { client.sendCommand(cmd) }
            if (result.isSuccess && req != null) runCatching { client.answerRequest(req.id, true) }
            main.post {
                busy = false
                render()
                result.onSuccess {
                    if (req != null) {
                        PushService.cancelRequest(this, req.id)
                        Toast.makeText(this, "已批准，几秒内生效", Toast.LENGTH_SHORT).show()
                        finish()
                        return@post
                    }
                    ui.say(statusView, Tone.GOOD, "已发给 ${target.label}：${cmd.body}\n几秒内执行，结果看推送消息。")
                }.onFailure { ui.say(statusView, Tone.BAD, "发送失败：${PushmeClient.describe(it)}") }
            }
        }
    }

    companion object {
        private const val EXTRA_TARGET = "target"
        private const val EXTRA_REQUEST = "request"

        /** 打开主动授权页，预先选好 [target]（调用方名，可空）。 */
        fun intent(context: Context, target: String?): Intent =
            Intent(context, GrantActivity::class.java).apply { if (target != null) putExtra(EXTRA_TARGET, target) }

        /** 打开批申请页（通知、主页点进来）。 */
        fun requestIntent(context: Context, id: String): Intent =
            Intent(context, GrantActivity::class.java).putExtra(EXTRA_REQUEST, id).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

        // 指令从签发到过期：listen 只认 5 分钟以内的，2 分钟足够送到
        private const val LIFE_SECONDS = 120L
        private const val DEFAULT_DURATION = "1h"
        private val DURATIONS = listOf(
            "30m" to "30 分钟", "1h" to "1 小时", "2h" to "2 小时",
            "4h" to "4 小时", "8h" to "8 小时", "12h" to "12 小时",
        )

        /** 对象没上报能选哪些机器时（旧的宿主 agent-grant）用这份：名字、提醒。 */
        private val HOSTS = listOf(
            HostOption("openwrt", "openwrt 是认证服务器，给它等于全部放开"),
            HostOption("unraid", null),
            HostOption("oracle", "oracle 宿主能自己续期"),
            HostOption("laptop", "laptop 上有你的证书，给它等于全部放开"),
            HostOption("termux-mi14", null),
        )
        private fun defaultHosts() = HOSTS

        /** 机器名的显示名：all 是「全部机器」，termux-mi14 是 mi14。 */
        fun hostLabel(name: String) = when (name) {
            "all" -> "全部机器"
            "termux-mi14" -> "mi14"
            else -> name
        }

        private const val ALL_NOTE = "全部机器：包括 pacvue、edge 设备等不能单选的，等于全部放开"
        private const val OTHERS_NOTE = "别的机器（pacvue、edge 设备等）只能选「全部」。"
    }
}
