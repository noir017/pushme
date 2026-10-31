package io.github.noir017.pushme

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.content.res.ColorStateList
import android.hardware.biometrics.BiometricManager.Authenticators
import android.hardware.biometrics.BiometricPrompt
import android.os.Bundle
import android.os.CancellationSignal
import android.os.Handler
import android.os.Looper
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.util.Base64
import android.view.Gravity
import android.view.View
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import android.widget.Toast
import java.util.concurrent.Executors

/**
 * 审批页：显示一条待审批的完整内容，[批准] 先过系统验证框（指纹或锁屏 PIN），通过后用本机密钥签名提交；
 * [拒绝] 不签名直接提交。内容以 `GET /approvals` 为准，签名覆盖标题和正文，pushme 改了内容发起方就验不过。
 *
 * 不在锁屏上显示（清单里没有 showWhenLocked），禁止截屏，被别的 App 的悬浮窗盖住时不接受点击（见 [Ui.screen]）。
 */
class ApprovalActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var contentBox: LinearLayout
    private lateinit var emptyBox: LinearLayout
    private lateinit var emptyView: TextView
    private lateinit var callerView: TextView
    private lateinit var titleView: TextView
    private lateinit var textView: TextView
    private lateinit var leftView: TextView
    private lateinit var leftBar: ProgressBar
    private lateinit var moreView: TextView
    private lateinit var statusView: TextView
    private lateinit var bottomBar: View
    private lateinit var approveButton: TextView
    private lateinit var denyButton: TextView

    private var approval: Approval? = null

    /** 服务器时间减本机时间（毫秒）。 */
    private var offsetMs = 0L
    private var busy = false

    private val tick = object : Runnable {
        override fun run() {
            renderLeft()
            main.postDelayed(this, 1_000)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        ui = Ui(this)
        callerView = ui.label(size = 14f, color = ui.muted)
        titleView = ui.label(size = 22f, bold = true)
        textView = ui.label(size = 16f).apply { setTextIsSelectable(true) }
        leftView = ui.label(size = 13f, color = ui.muted)
        leftBar = ProgressBar(this, null, android.R.attr.progressBarStyleHorizontal).apply {
            max = 1000
            progressBackgroundTintList = ColorStateList.valueOf(ui.surface2)
        }
        moreView = ui.label(size = 13f, color = ui.muted)
        statusView = ui.notice()
        emptyView = ui.label(size = 16f, color = ui.muted).apply { gravity = Gravity.CENTER }
        denyButton = ui.button("拒绝", Btn.OUTLINE, ui.bad) { deny() }
        approveButton = ui.button("验证并批准", Btn.FILLED, ui.good) { approve() }

        contentBox = ui.column(
            ui.column(callerView, titleView, gap = 4),
            ui.column(leftView, leftBar, gap = 4),
            ui.card(textView),
            ui.label("批准要验证指纹或锁屏密码，用本机密钥签名后才算数；拒绝不用验证。", 13f, ui.muted),
            moreView,
            gap = 14,
        )
        emptyBox = ui.column(emptyView, ui.button("返回", Btn.TONAL) { finish() }, gap = 24).apply {
            setPadding(0, ui.dp(72), 0, 0)
        }
        bottomBar = ui.bottomBar(statusView, ui.row(denyButton, approveButton, weights = listOf(1f, 1.6f)))
        ui.screen("审批", ui.column(contentBox, emptyBox), bottom = bottomBar, secure = true)
        show(null, "加载中…")
        load(intent.getStringExtra(EXTRA_ID))
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        if (!busy) load(intent.getStringExtra(EXTRA_ID))
    }

    override fun onResume() {
        super.onResume()
        main.post(tick)
    }

    override fun onPause() {
        main.removeCallbacks(tick)
        super.onPause()
    }

    override fun onDestroy() {
        io.shutdownNow()
        super.onDestroy()
    }

    /** 取待审批，显示 [id] 那条；[id] 为空显示最早的一条。 */
    private fun load(id: String?) {
        if (!prefs.configured) {
            show(null, "还没配置 pushme：回主页点「设置」填地址和接收方 token")
            return
        }
        say(Tone.PLAIN, "加载中…")
        setButtons(false)
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.pending() }
            main.post {
                if (isFinishing) return@post
                result.onSuccess { p ->
                    offsetMs = if (p.now > 0) p.now * 1000 - System.currentTimeMillis() else 0
                    val a = if (id != null) p.approvals.find { it.id == id } else p.approvals.firstOrNull()
                    if (a == null && id != null) PushService.cancelApproval(this, id)
                    show(a, if (a == null) (if (id != null) "这条审批已经处理过或已过期" else "没有待审批") else "", p.approvals.size - 1)
                }.onFailure { show(null, "取审批失败：${PushmeClient.describe(it)}", tone = Tone.BAD) }
            }
        }
    }

    /** 显示 [a]（为空就显示 [status] 和返回键）；[others] 是除它以外还有几条待审批。 */
    private fun show(a: Approval?, status: String, others: Int = 0, tone: Tone = Tone.INFO) {
        approval = a
        contentBox.visibility = if (a != null) View.VISIBLE else View.GONE
        bottomBar.visibility = if (a != null) View.VISIBLE else View.GONE
        emptyBox.visibility = if (a == null) View.VISIBLE else View.GONE
        if (a != null) {
            callerView.text = "来自 ${a.caller}"
            titleView.text = a.title.ifBlank { "（无标题）" }
            textView.text = a.text
            moreView.text = if (others > 0) "另外还有 $others 条待审批，处理完这条接着显示。" else ""
            moreView.visibility = if (others > 0) View.VISIBLE else View.GONE
        }
        say(tone, status)
        setButtons(a != null && leftMs(a) > 0)
        renderLeft()
    }

    /** 状态提示：有审批时显示在按钮上方，没有时显示在页中间。 */
    private fun say(tone: Tone, text: String) {
        if (approval != null) {
            ui.say(statusView, tone, text)
        } else {
            emptyView.text = text
            emptyView.setTextColor(if (tone == Tone.BAD) ui.bad else ui.muted)
        }
    }

    private fun leftMs(a: Approval) = a.expires * 1000 - offsetMs - System.currentTimeMillis()

    private fun renderLeft() {
        val a = approval ?: return
        val left = leftMs(a)
        if (left <= 0) {
            leftView.text = "已过期"
            leftView.setTextColor(ui.bad)
            leftBar.progress = 0
            if (!busy) setButtons(false)
            return
        }
        val s = left / 1000
        val urgent = left < URGENT_MS
        leftView.text = "还剩 %d:%02d".format(s / 60, s % 60)
        leftView.setTextColor(if (urgent) ui.bad else ui.muted)
        val total = (a.expires - a.created) * 1000
        leftBar.progress = if (total > 0) (left * 1000 / total).toInt().coerceIn(0, 1000) else 1000
        leftBar.progressTintList = ColorStateList.valueOf(if (urgent) ui.bad else ui.primary)
    }

    private fun approve() {
        val a = approval ?: return
        if (busy) return
        val signer = try {
            Approver.signer()
        } catch (e: KeyPermanentlyInvalidatedException) {
            say(Tone.BAD, "本机密钥已被系统作废（删过锁屏密码？）：到设置里重新生成密钥，再重新配对")
            return
        } catch (e: Exception) {
            say(Tone.BAD, "用不了本机密钥：${e.message ?: e.javaClass.simpleName}。到设置里看看有没有生成密钥")
            return
        }
        val prompt = BiometricPrompt.Builder(this)
            .setTitle("批准：" + a.title.ifBlank { a.caller })
            .setSubtitle("来自 ${a.caller}")
            .setDescription(a.text.take(PROMPT_TEXT))
            .setAllowedAuthenticators(Authenticators.BIOMETRIC_STRONG or Authenticators.DEVICE_CREDENTIAL)
            .build()
        prompt.authenticate(
            BiometricPrompt.CryptoObject(signer), CancellationSignal(), mainExecutor,
            object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                    val sig = try {
                        val s = result.cryptoObject?.signature ?: throw IllegalStateException("没拿到签名器")
                        s.update(Approver.payload(a))
                        Base64.encodeToString(s.sign(), Base64.NO_WRAP)
                    } catch (e: Exception) {
                        say(Tone.BAD, "签名失败：${e.message ?: e.javaClass.simpleName}")
                        return
                    }
                    send(a, sig)
                }

                override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                    say(Tone.WARN, "没有批准：$errString")
                }
            },
        )
    }

    private fun deny() {
        val a = approval ?: return
        if (!busy) send(a, null)
    }

    /** 提交答复：[sig] 为 null 表示拒绝。 */
    private fun send(a: Approval, sig: String?) {
        busy = true
        setButtons(false)
        say(Tone.INFO, "提交中…")
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.answer(a.id, sig) }
            main.post {
                busy = false
                result.onSuccess {
                    PushService.cancelApproval(this, a.id)
                    Toast.makeText(this, if (sig != null) "已批准" else "已拒绝", Toast.LENGTH_SHORT).show()
                    showNextOrFinish()
                }.onFailure { e ->
                    say(Tone.BAD, "提交失败：${PushmeClient.describe(e)}")
                    if (e is ApiError && e.code == 409) {
                        PushService.cancelApproval(this, a.id)
                    } else {
                        setButtons(leftMs(a) > 0)
                    }
                }
            }
        }
    }

    /** 处理完一条：还有别的待审批就接着显示，没有就关掉。 */
    private fun showNextOrFinish() {
        approval = null
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val next = runCatching { client.pending() }.getOrNull()
            main.post {
                if (isFinishing) return@post
                val a = next?.approvals?.firstOrNull()
                if (a == null) {
                    finish()
                } else {
                    offsetMs = if (next.now > 0) next.now * 1000 - System.currentTimeMillis() else 0
                    show(a, "上一条已提交，这是下一条待审批", next.approvals.size - 1)
                }
            }
        }
    }

    private fun setButtons(enabled: Boolean) {
        for (b in listOf(approveButton, denyButton)) b.enable(enabled)
    }

    companion object {
        private const val EXTRA_ID = "id"
        // 系统验证框里的正文只放前面这些字，完整的在页面上
        private const val PROMPT_TEXT = 300
        // 剩下不到这么久，倒计时变红
        private const val URGENT_MS = 60_000L

        fun intent(context: Context, id: String?): Intent =
            Intent(context, ApprovalActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                .apply { if (id != null) putExtra(EXTRA_ID, id) }
    }
}
