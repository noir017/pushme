package io.github.noir017.pushme

import android.Manifest
import android.app.Activity
import android.app.AlertDialog
import android.app.NotificationManager
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
import android.view.Gravity
import android.view.View
import android.view.inputmethod.InputMethodManager
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import java.util.concurrent.Executors

/**
 * 设置：pushme 地址与接收方 token、审批密钥（生成、复制 / 分享公钥）、通知与后台运行的权限。
 */
class SettingsActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var urlEdit: EditText
    private lateinit var tokenEdit: EditText
    private lateinit var status: Ui.StatusLine
    private lateinit var keyBox: LinearLayout
    private lateinit var notifyState: TextView
    private lateinit var batteryState: TextView
    private var generating = false

    private val tick = object : Runnable {
        override fun run() {
            status.set(ui.linkTone(PushService.link), PushService.status)
            main.postDelayed(this, 1_000)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        ui = Ui(this)
        urlEdit = ui.field("如 https://pushme.lan.noharanas.eu.org", prefs.url)
        tokenEdit = ui.field("PUSHME_RECEIVERS 里的一项", prefs.token, password = true)
        status = ui.StatusLine()
        keyBox = ui.column()
        val (notifyRow, notify) = ui.item("通知", "小米 / HyperOS：给「审批」「消息」打开悬浮通知") { requestNotifications() }
        val (batteryRow, battery) = ui.item("后台不限制耗电", "长时间锁屏后也能收到") { requestIgnoreBattery() }
        val (autostartRow, autostart) = ui.item("自启动", "小米 / HyperOS 要在应用详情里手动打开") {
            startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")))
        }
        notifyState = notify
        batteryState = battery
        mark(autostart, false, "", "去设置 ›")

        val body = ui.column(
            ui.header("连接"),
            ui.card(
                ui.column(ui.label("pushme 地址", 13f, ui.muted), urlEdit, gap = 6),
                ui.column(ui.label("接收方 token", 13f, ui.muted), tokenEdit, gap = 6),
                status.view,
                ui.row(
                    ui.button("断开", Btn.OUTLINE, ui.ink) { stopService(Intent(this, PushService::class.java)) },
                    ui.button("保存并连接") { save() },
                    weights = listOf(1f, 2f),
                ),
                gap = 14,
            ),
            ui.header("审批密钥"),
            ui.card(keyBox),
            ui.header("后台与通知"),
            ui.list(notifyRow, batteryRow, autostartRow),
            ui.label("pushme " + packageManager.getPackageInfo(packageName, 0).versionName, 12f, ui.muted).apply {
                gravity = Gravity.CENTER
                setPadding(0, ui.dp(24), 0, 0)
            },
        )
        ui.screen("设置", body)
        renderKey()
    }

    override fun onResume() {
        super.onResume()
        renderPermissions()
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

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        renderPermissions()
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
        getSystemService(InputMethodManager::class.java).hideSoftInputFromWindow(tokenEdit.windowToken, 0)
        currentFocus?.clearFocus()
        toast("已保存")
    }

    private fun renderKey() {
        keyBox.removeAllViews()
        val put = { v: View -> ui.add(keyBox, v, top = if (keyBox.childCount == 0) 0 else 10) }
        val fp = runCatching { Approver.fingerprint() }.getOrNull()
        if (fp == null) {
            put(ui.label("还没有密钥", 16f, bold = true))
            put(ui.label("生成后才能批准审批、主动授权。要先在系统设置里设好锁屏密码。", 14f, ui.muted))
            put(ui.button(if (generating) "生成中…" else "生成密钥") { generate() }.apply { enable(!generating) })
            return
        }
        put(ui.label("公钥指纹", 13f, ui.muted))
        put(ui.label(fp, 22f).apply {
            typeface = Typeface.create(Typeface.MONOSPACE, Typeface.BOLD)
            setTextIsSelectable(true)
        })
        put(ui.label("配对：把公钥装到发起审批的机器上，用 pushme fingerprint 核对两边指纹一致。这一步要你自己做，别让别人代传公钥。", 13f, ui.muted))
        put(ui.row(
            ui.button("复制公钥", Btn.TONAL) { copyPublicKey() },
            ui.button("分享公钥", Btn.TONAL) { sharePublicKey() },
        ))
        put(ui.button(if (generating) "生成中…" else "重新生成密钥", Btn.TEXT, ui.bad) { confirmGenerate() }.apply { enable(!generating) })
    }

    private fun confirmGenerate() {
        AlertDialog.Builder(this)
            .setTitle("重新生成密钥？")
            .setMessage("旧密钥会被删除，之前配对过的机器都要重新装公钥，否则验不过你的批准。")
            .setPositiveButton("重新生成") { _, _ -> generate() }
            .setNegativeButton("取消", null)
            .show()
    }

    private fun generate() {
        if (generating) return
        generating = true
        renderKey()
        io.execute {
            val result = runCatching { Approver.generate() }
            main.post {
                generating = false
                if (isDestroyed) return@post
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

    private fun renderPermissions() {
        val notifyOn = checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED &&
            getSystemService(NotificationManager::class.java).areNotificationsEnabled()
        mark(notifyState, notifyOn, "已开启", "去开启 ›")
        mark(batteryState, getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName), "已允许", "去设置 ›")
    }

    /** 条目右侧：已办好显示绿色的 [done]，没办好显示蓝色的 [todo]。 */
    private fun mark(view: TextView, ok: Boolean, done: String, todo: String) {
        view.text = if (ok) done else todo
        view.setTextColor(if (ok) ui.good else ui.primary)
    }

    /** 没给权限就申请；给过了打开通知设置，方便给各个通道开悬浮通知。 */
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

    private fun toast(text: String) = Toast.makeText(this, text, Toast.LENGTH_LONG).show()
}
