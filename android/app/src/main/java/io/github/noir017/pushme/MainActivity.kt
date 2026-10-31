package io.github.noir017.pushme

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Typeface
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.provider.Settings
import android.text.InputType
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import java.util.concurrent.Executors

/**
 * 主界面：填 pushme 地址与接收方 token、看连接状态；生成审批密钥、导出公钥配对；待审批入口；常用权限设置。
 */
class MainActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var statusView: TextView
    private lateinit var urlEdit: EditText
    private lateinit var tokenEdit: EditText
    private lateinit var keyView: TextView
    private lateinit var keyButton: Button

    private val tick = object : Runnable {
        override fun run() {
            statusView.text = "状态：${PushService.status}"
            main.postDelayed(this, 1_000)
        }
    }

    private val density by lazy { resources.displayMetrics.density }
    private fun dp(value: Int) = (value * density).toInt()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        statusView = TextView(this).apply { textSize = 15f }
        urlEdit = EditText(this).apply {
            hint = "pushme 地址，如 http://192.168.0.185:8290"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI
            setText(prefs.url)
        }
        tokenEdit = EditText(this).apply {
            hint = "接收方 token（PUSHME_RECEIVERS 里的一项）"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            setText(prefs.token)
        }
        keyView = TextView(this).apply {
            textSize = 14f
            setTextIsSelectable(true)
        }
        keyButton = button("") { confirmGenerate() }

        val layout = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(dp(16), dp(16), dp(16), dp(24))
            addView(statusView)
            addView(row(button("查看待审批") { startActivity(ApprovalActivity.intent(this@MainActivity, null)) }))

            addView(section("连接"))
            addView(urlEdit)
            addView(tokenEdit)
            addView(row(button("保存并连接") { save() }, button("停止服务") { stopService(Intent(this@MainActivity, PushService::class.java)) }))

            addView(section("审批密钥"))
            addView(keyView)
            addView(row(keyButton))
            addView(row(button("复制公钥") { copyPublicKey() }, button("分享公钥") { sharePublicKey() }))
            addView(note("配对：把公钥装到发起审批的机器上，再核对两边显示的指纹一致。这一步要你自己做，别让别人代传公钥。"))

            addView(section("权限"))
            addView(row(button("允许通知") { requestNotifications() }, button("不限制后台耗电") { requestIgnoreBattery() }))
            addView(note("小米 / HyperOS：另在系统设置里给 pushme 打开「自启动」，并在通知设置里给「审批」「消息」打开悬浮通知。"))

            addView(note("版本 " + packageManager.getPackageInfo(packageName, 0).versionName))
        }
        setContentView(ScrollView(this).apply {
            fitsSystemWindows = true
            addView(layout)
        })
        renderKey()
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        if (prefs.configured) PushService.start(this)
    }

    override fun onResume() {
        super.onResume()
        main.post(tick)
    }

    override fun onPause() {
        main.removeCallbacks(tick)
        super.onPause()
    }

    private fun save() {
        val url = urlEdit.text.toString().trim().trimEnd('/')
        val token = tokenEdit.text.toString().trim()
        if (!url.startsWith("http://") && !url.startsWith("https://")) {
            toast("地址要以 http:// 或 https:// 开头")
            return
        }
        if (token.length < 16) {
            toast("token 至少 16 位")
            return
        }
        if (url != prefs.url || token != prefs.token) {
            prefs.url = url
            prefs.token = token
            prefs.cursor = -1 // 换了收件箱：从它当前的序号收起，不回放历史
            stopService(Intent(this, PushService::class.java)) // 让服务按新配置重连
        }
        PushService.start(this)
        toast("已保存")
    }

    private fun renderKey() {
        val fp = runCatching { Approver.fingerprint() }.getOrNull()
        keyView.text = if (fp == null) "还没有密钥：生成后才能批准审批。需要先设好锁屏密码。" else "公钥指纹：$fp"
        keyButton.text = if (fp == null) "生成密钥" else "重新生成密钥"
    }

    private fun confirmGenerate() {
        if (!Approver.hasKey()) {
            generate()
            return
        }
        AlertDialog.Builder(this)
            .setTitle("重新生成密钥？")
            .setMessage("旧密钥会被删除，之前配对过的机器都要重新装公钥，否则验不过你的批准。")
            .setPositiveButton("重新生成") { _, _ -> generate() }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun generate() {
        keyButton.isEnabled = false
        io.execute {
            val result = runCatching { Approver.generate() }
            main.post {
                keyButton.isEnabled = true
                result.onSuccess { strongBox -> toast(if (strongBox) "已生成（在安全芯片里）" else "已生成（手机没有安全芯片，放在系统密钥库）") }
                    .onFailure { toast("生成失败：${it.message ?: it.javaClass.simpleName}（先在系统设置里设好锁屏密码）") }
                renderKey()
            }
        }
    }

    private fun copyPublicKey() {
        val pem = Approver.publicKeyPem() ?: return toast("还没有密钥")
        getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("pushme 公钥", pem))
        toast("已复制公钥，指纹 ${Approver.fingerprint()}")
    }

    private fun sharePublicKey() {
        val pem = Approver.publicKeyPem() ?: return toast("还没有密钥")
        val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, pem)
        startActivity(Intent.createChooser(send, "分享公钥"))
    }

    private fun requestNotifications() {
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) {
            startActivity(Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, packageName))
        } else {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
    }

    private fun requestIgnoreBattery() {
        if (getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)) {
            toast("已经不受电池优化限制")
            return
        }
        startActivity(Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName")))
    }

    private fun button(label: String, onClick: () -> Unit) = Button(this).apply {
        text = label
        isAllCaps = false
        setOnClickListener { onClick() }
    }

    private fun row(vararg buttons: Button) = LinearLayout(this).apply {
        orientation = LinearLayout.HORIZONTAL
        for (b in buttons) addView(b, LinearLayout.LayoutParams(0, ViewGroup.LayoutParams.WRAP_CONTENT, 1f))
    }

    private fun section(title: String) = TextView(this).apply {
        text = title
        textSize = 16f
        typeface = Typeface.DEFAULT_BOLD
        setPadding(0, dp(20), 0, dp(4))
    }

    private fun note(text: String) = TextView(this).apply {
        this.text = text
        textSize = 12f
        setTextColor(GREY)
        setPadding(0, dp(4), 0, 0)
    }

    private fun toast(text: String) = Toast.makeText(this, text, Toast.LENGTH_LONG).show()

    companion object {
        private const val GREY = 0xFF757575.toInt()
    }
}
