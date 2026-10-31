package io.github.noir017.pushme

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.graphics.Color
import android.graphics.Typeface
import android.hardware.biometrics.BiometricManager.Authenticators
import android.hardware.biometrics.BiometricPrompt
import android.os.Bundle
import android.os.CancellationSignal
import android.os.Handler
import android.os.Looper
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.util.Base64
import android.view.Gravity
import android.view.ViewGroup
import android.view.WindowManager
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import java.util.concurrent.Executors

/**
 * 审批页：显示一条待审批的完整内容，[批准] 先过系统验证框（指纹或锁屏 PIN），通过后用本机密钥签名提交；
 * [拒绝] 不签名直接提交。内容以 `GET /approvals` 为准，签名覆盖标题和正文，pushme 改了内容发起方就验不过。
 *
 * 不在锁屏上显示（清单里没有 showWhenLocked），禁止截屏，被别的 App 的悬浮窗盖住时不接受点击。
 */
class ApprovalActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var callerView: TextView
    private lateinit var titleView: TextView
    private lateinit var textView: TextView
    private lateinit var leftView: TextView
    private lateinit var statusView: TextView
    private lateinit var approveButton: Button
    private lateinit var denyButton: Button

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

    private val density by lazy { resources.displayMetrics.density }
    private fun dp(value: Int) = (value * density).toInt()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // 审批内容不进截屏、录屏和最近任务的缩略图
        window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
        prefs = Prefs(this)
        callerView = TextView(this).apply {
            textSize = 13f
            setTextColor(GREY)
        }
        titleView = TextView(this).apply {
            textSize = 20f
            typeface = Typeface.DEFAULT_BOLD
            setPadding(0, dp(4), 0, dp(8))
        }
        textView = TextView(this).apply {
            textSize = 16f
            setTextIsSelectable(true)
        }
        leftView = TextView(this).apply {
            textSize = 13f
            setTextColor(GREY)
            setPadding(0, dp(12), 0, 0)
        }
        statusView = TextView(this).apply {
            textSize = 14f
            setPadding(0, dp(12), 0, 0)
        }
        denyButton = Button(this).apply {
            text = "拒绝"
            setOnClickListener { deny() }
        }
        approveButton = Button(this).apply {
            text = "批准"
            setTextColor(Color.WHITE)
            setBackgroundColor(APPROVE_BG)
            setOnClickListener { approve() }
        }
        val buttons = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            setPadding(0, dp(20), 0, 0)
            addView(denyButton, weighted())
            addView(approveButton, weighted())
        }
        val layout = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(20), dp(20), dp(20), dp(24))
            addView(callerView)
            addView(titleView)
            addView(textView)
            addView(leftView)
            addView(buttons)
            addView(statusView)
        }
        setContentView(ScrollView(this).apply {
            fitsSystemWindows = true
            // 被别的 App 的悬浮窗盖住时丢掉点击：防止有人用透明悬浮窗骗你点「批准」
            filterTouchesWhenObscured = true
            addView(layout)
        })
        setButtons(false)
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
            show(null, "还没配置 pushme：回主界面填地址和接收方 token")
            return
        }
        statusView.text = "加载中…"
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
                    show(a, if (a == null) (if (id != null) "这条审批已经处理过或已过期" else "没有待审批") else "")
                }.onFailure { show(null, "取审批失败：${PushmeClient.describe(it)}") }
            }
        }
    }

    private fun show(a: Approval?, status: String) {
        approval = a
        callerView.text = a?.let { "来自 ${it.caller}" } ?: ""
        titleView.text = a?.title?.ifBlank { "（无标题）" } ?: ""
        textView.text = a?.text ?: ""
        statusView.text = status
        setButtons(a != null && leftMs(a) > 0)
        renderLeft()
    }

    private fun leftMs(a: Approval) = a.expires * 1000 - offsetMs - System.currentTimeMillis()

    private fun renderLeft() {
        val a = approval ?: run {
            leftView.text = ""
            return
        }
        val left = leftMs(a)
        if (left <= 0) {
            leftView.text = "已过期"
            if (!busy) setButtons(false)
            return
        }
        val s = left / 1000
        leftView.text = "还剩 %d:%02d".format(s / 60, s % 60)
    }

    private fun approve() {
        val a = approval ?: return
        if (busy) return
        val signer = try {
            Approver.signer()
        } catch (e: KeyPermanentlyInvalidatedException) {
            statusView.text = "本机密钥已被系统作废（删过锁屏密码？）：回主界面重新生成密钥，再重新配对"
            return
        } catch (e: Exception) {
            statusView.text = "用不了本机密钥：${e.message ?: e.javaClass.simpleName}。回主界面看看有没有生成密钥"
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
                        statusView.text = "签名失败：${e.message ?: e.javaClass.simpleName}"
                        return
                    }
                    send(a, sig)
                }

                override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                    statusView.text = "没有批准：$errString"
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
        statusView.text = "提交中…"
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.answer(a.id, sig) }
            main.post {
                busy = false
                result.onSuccess {
                    PushService.cancelApproval(this, a.id)
                    Toast.makeText(this, if (sig != null) "已批准" else "已拒绝", Toast.LENGTH_SHORT).show()
                    finish()
                }.onFailure { e ->
                    statusView.text = "提交失败：${PushmeClient.describe(e)}"
                    if (e is ApiError && e.code == 409) {
                        PushService.cancelApproval(this, a.id)
                    } else {
                        setButtons(leftMs(a) > 0)
                    }
                }
            }
        }
    }

    private fun setButtons(enabled: Boolean) {
        for (b in listOf(approveButton, denyButton)) {
            b.isEnabled = enabled
            b.alpha = if (enabled) 1f else 0.4f
        }
    }

    private fun weighted() = LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f).apply {
        marginStart = dp(4)
        marginEnd = dp(4)
        gravity = Gravity.CENTER
    }

    companion object {
        private const val EXTRA_ID = "id"
        private const val GREY = 0xFF757575.toInt()
        private const val APPROVE_BG = 0xFF2E7D32.toInt()
        // 系统验证框里的正文只放前面这些字，完整的在页面上
        private const val PROMPT_TEXT = 300

        fun intent(context: Context, id: String?): Intent =
            Intent(context, ApprovalActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                .apply { if (id != null) putExtra(EXTRA_ID, id) }
    }
}
