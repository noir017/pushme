package io.github.noir017.pushme

import android.app.Activity
import android.app.AlertDialog
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
import android.view.ViewGroup
import android.view.WindowManager
import android.widget.Button
import android.widget.CheckBox
import android.widget.LinearLayout
import android.widget.RadioButton
import android.widget.RadioGroup
import android.widget.ScrollView
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
    private lateinit var targetGroup: RadioGroup
    private lateinit var allBox: CheckBox
    private lateinit var hostBoxes: List<Pair<String, CheckBox>>
    private lateinit var durGroup: RadioGroup
    private lateinit var statusView: TextView
    private lateinit var grantButton: Button
    private lateinit var revokeButton: Button

    private var targets: List<CommandTarget> = emptyList()

    /** 服务器时间减本机时间（毫秒）：签发时刻按服务器时间算，手机时钟不准也不会被当成过期。 */
    private var offsetMs = 0L
    private var busy = false

    private val density by lazy { resources.displayMetrics.density }
    private fun dp(value: Int) = (value * density).toInt()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
        prefs = Prefs(this)
        title = "主动授权"

        targetGroup = RadioGroup(this)
        allBox = CheckBox(this).apply {
            text = "全部机器（等于全部放开）"
            setOnCheckedChangeListener { _, checked -> hostBoxes.forEach { it.second.isEnabled = !checked } }
        }
        hostBoxes = HOSTS.map { (name, label) -> name to CheckBox(this).apply { text = label } }
        durGroup = RadioGroup(this).apply { orientation = RadioGroup.HORIZONTAL }
        for (d in DURATIONS) {
            durGroup.addView(RadioButton(this).apply {
                text = d
                id = ViewGroup.generateViewId()
                tag = d
                if (d == DEFAULT_DURATION) isChecked = true
            })
        }
        statusView = TextView(this).apply {
            textSize = 14f
            setPadding(0, dp(12), 0, 0)
        }
        revokeButton = Button(this).apply {
            text = "收回全部"
            setOnClickListener { confirmRevoke() }
        }
        grantButton = Button(this).apply {
            text = "授权"
            setTextColor(Color.WHITE)
            setBackgroundColor(GRANT_BG)
            setOnClickListener { grant() }
        }

        val layout = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(16), dp(16), dp(16), dp(24))
            addView(section("给谁"))
            addView(targetGroup)
            addView(section("哪些机器"))
            addView(allBox)
            hostBoxes.forEach { addView(it.second) }
            addView(note("别的机器（pacvue、edge 设备等）只能选「全部机器」。"))
            addView(section("多久"))
            addView(ScrollView(this@GrantActivity).apply { addView(durGroup) })
            addView(LinearLayout(this@GrantActivity).apply {
                orientation = LinearLayout.HORIZONTAL
                setPadding(0, dp(20), 0, 0)
                addView(revokeButton, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
                addView(grantButton, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
            })
            addView(statusView)
        }
        setContentView(ScrollView(this).apply {
            fitsSystemWindows = true
            filterTouchesWhenObscured = true
            addView(layout)
        })
        loadTargets()
    }

    override fun onDestroy() {
        io.shutdownNow()
        super.onDestroy()
    }

    private fun loadTargets() {
        if (!prefs.configured) {
            statusView.text = "还没配置 pushme：回主界面填地址和接收方 token"
            setButtons(false)
            return
        }
        statusView.text = "加载在线的 agent…"
        setButtons(false)
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.targets() }
            main.post {
                if (isFinishing) return@post
                result.onSuccess { t ->
                    offsetMs = if (t.now > 0) t.now * 1000 - System.currentTimeMillis() else 0
                    targets = t.targets
                    targetGroup.removeAllViews()
                    for (x in t.targets) {
                        targetGroup.addView(RadioButton(this).apply {
                            text = if (x.label == x.name) x.label else "${x.label}（${x.name}）"
                            id = ViewGroup.generateViewId()
                            tag = x.name
                        })
                    }
                    if (t.targets.size == 1) (targetGroup.getChildAt(0) as RadioButton).isChecked = true
                    statusView.text = if (t.targets.isEmpty()) "没有在线的 agent：宿主上的 pushme listen 没在跑" else ""
                    setButtons(t.targets.isNotEmpty())
                }.onFailure { statusView.text = "取在线名单失败：${PushmeClient.describe(it)}" }
            }
        }
    }

    private fun selectedTarget(): CommandTarget? {
        val name = targetGroup.findViewById<RadioButton>(targetGroup.checkedRadioButtonId)?.tag as? String ?: return null
        return targets.find { it.name == name }
    }

    private fun grant() {
        val target = selectedTarget() ?: return toastStatus("先选给谁")
        val hosts = if (allBox.isChecked) "all" else hostBoxes.filter { it.second.isChecked }.joinToString(",") { it.first }
        if (hosts.isEmpty()) return toastStatus("至少选一台机器，或「全部机器」")
        val dur = durGroup.findViewById<RadioButton>(durGroup.checkedRadioButtonId)?.tag as? String ?: DEFAULT_DURATION
        val what = if (hosts == "all") "全部机器" else hosts.replace("termux-mi14", "mi14").replace(",", "、")
        sign(target, "grant $hosts $dur", "授权 ${target.label}：$what $dur")
    }

    private fun confirmRevoke() {
        val target = selectedTarget() ?: return toastStatus("先选给谁")
        AlertDialog.Builder(this)
            .setTitle("收回 ${target.label} 的全部提权？")
            .setMessage("所有机器的授权立刻收回（已复制走的证书最多再活 30 分钟）。")
            .setPositiveButton("收回") { _, _ -> sign(target, "revoke", "收回 ${target.label} 的全部提权") }
            .setNegativeButton("取消", null)
            .show()
    }

    /** 过系统验证框后签名并投递。 */
    private fun sign(target: CommandTarget, body: String, promptTitle: String) {
        if (busy) return
        val signer = try {
            Approver.signer()
        } catch (e: KeyPermanentlyInvalidatedException) {
            return toastStatus("本机密钥已被系统作废：回主界面重新生成密钥，再重新配对")
        } catch (e: Exception) {
            return toastStatus("用不了本机密钥：${e.message ?: e.javaClass.simpleName}")
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
                            return toastStatus("签名失败：${e.message ?: e.javaClass.simpleName}")
                        }
                        send(cmd.copy(sig = sig), target)
                    }

                    override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
                        statusView.text = "没有发出：$errString"
                    }
                },
            )
    }

    private fun send(cmd: SignedCommand, target: CommandTarget) {
        busy = true
        setButtons(false)
        statusView.text = "发送中…"
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val result = runCatching { client.sendCommand(cmd) }
            main.post {
                busy = false
                setButtons(true)
                result.onSuccess {
                    statusView.text = "已发给 ${target.label}：${cmd.body}\n宿主几秒内执行，结果看推送消息（「agent 获得提权…」/「已手动收回」）。"
                }.onFailure { statusView.text = "发送失败：${PushmeClient.describe(it)}" }
            }
        }
    }

    private fun toastStatus(text: String) {
        statusView.text = text
    }

    private fun setButtons(enabled: Boolean) {
        for (b in listOf(grantButton, revokeButton)) {
            b.isEnabled = enabled
            b.alpha = if (enabled) 1f else 0.4f
        }
    }

    private fun section(title: String) = TextView(this).apply {
        text = title
        textSize = 16f
        typeface = Typeface.DEFAULT_BOLD
        setPadding(0, dp(16), 0, dp(4))
    }

    private fun note(text: String) = TextView(this).apply {
        this.text = text
        textSize = 12f
        setTextColor(GREY)
    }

    companion object {
        private const val GREY = 0xFF757575.toInt()
        private const val GRANT_BG = 0xFF2E7D32.toInt()
        // 指令从签发到过期：宿主 listen 只认 5 分钟以内的，2 分钟足够送到
        private const val LIFE_SECONDS = 120L
        private const val DEFAULT_DURATION = "1h"
        private val DURATIONS = listOf("30m", "1h", "2h", "4h", "8h", "12h")

        /** 能单台授权的机器（宿主上建了 grant-<节点名> 角色的）：节点名 to 显示名。 */
        private val HOSTS = listOf(
            "openwrt" to "openwrt（认证服务器，等于全部放开）",
            "unraid" to "unraid",
            "oracle" to "oracle 宿主（能自己续期）",
            "laptop" to "laptop（有你的证书，等于全部放开）",
            "termux-mi14" to "mi14（Termux）",
        )
    }
}
