package io.github.noir017.pushme

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Color
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.view.View
import android.widget.LinearLayout
import android.widget.TextView
import java.util.concurrent.Executors

/**
 * 主页：连接状态、待审批（点开去审批）、各 agent 当前的授权（宿主上报的 [GrantStatus]，点卡片去开授权或收回）。
 * 消息在 [MessagesActivity]，地址、token、密钥、权限在 [SettingsActivity]。
 *
 * 在前台时每几秒查一次待审批和授权状态；有没自动弹过的待审批就直接打开审批页（通知不一定弹得出来，进 App 就能处理）。
 * 待审批的正文只在审批页（禁截屏）里显示，主页只露标题。
 */
class MainActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var status: Ui.StatusLine
    private lateinit var setupBox: LinearLayout
    private lateinit var pendingBox: LinearLayout
    private lateinit var grantBox: LinearLayout

    private var pending: List<Approval> = emptyList()

    /** 在线的 agent 及其上报的授权；null 表示还没取到。 */
    private var targets: List<CommandTarget>? = null
    private var targetsError: String? = null

    /** 每秒刷新的倒计时文字：待审批的、授权的分开放，各自重画时清掉自己那份。 */
    private val pendingCountdowns = ArrayList<Pair<TextView, () -> String>>()
    private val grantCountdowns = ArrayList<Pair<TextView, () -> String>>()

    /** 服务器时间减本机时间（毫秒）。 */
    private var offsetMs = 0L

    private var lastRefresh = 0L
    private var refreshing = false

    private val tick = object : Runnable {
        override fun run() {
            renderStatus()
            for ((view, text) in pendingCountdowns + grantCountdowns) view.text = text()
            if (System.currentTimeMillis() - lastRefresh >= REFRESH_EVERY_MS) refresh()
            main.postDelayed(this, 1_000)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        ui = Ui(this)
        status = ui.StatusLine()
        setupBox = ui.column()
        pendingBox = ui.column()
        grantBox = ui.column()
        val body = ui.column(ui.card(status.view, onClick = { openSettings() }))
        ui.add(body, setupBox)
        ui.add(body, pendingBox)
        ui.add(body, grantBox)
        val actions = ui.row(
            ui.button("消息", Btn.TEXT) { startActivity(Intent(this, MessagesActivity::class.java)) },
            ui.button("设置", Btn.TEXT) { openSettings() },
            weights = listOf(0f, 0f),
            gap = 0,
        )
        ui.screen("pushme", body, back = false, action = actions)
        renderGrants()

        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        if (prefs.configured) PushService.start(this)
    }

    override fun onResume() {
        super.onResume()
        renderSetup()
        lastRefresh = 0 // 回到前台立刻查一次
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

    private fun openSettings() = startActivity(Intent(this, SettingsActivity::class.java))

    private fun serverNow() = (System.currentTimeMillis() + offsetMs) / 1000

    private fun renderStatus() {
        val link = PushService.link
        val host = runCatching { Uri.parse(prefs.url).host }.getOrNull()
        status.set(ui.linkTone(link), if (link == PushService.Link.ONLINE && host != null) "${PushService.status} · $host" else PushService.status)
    }

    /** 没配好时的引导：先连上 pushme，再生成密钥。 */
    private fun renderSetup() {
        setupBox.removeAllViews()
        val hasKey = runCatching { Approver.hasKey() }.getOrDefault(false)
        val (title, text, action) = when {
            !prefs.configured -> Triple("先连上 pushme", "填好 pushme 地址和接收方 token，才能收消息、做审批。", "去设置")
            !hasKey -> Triple("还没有审批密钥", "生成密钥、把公钥装到发起审批的机器上，才能批准审批和主动授权。", "去生成")
            else -> return
        }
        ui.add(setupBox, ui.card(
            ui.label(title, 17f, bold = true),
            ui.label(text, 14f, ui.muted),
            ui.button(action) { openSettings() },
            gap = 10,
            stroke = ui.alpha(ui.primary, 0x80),
        ), top = 12)
    }

    /** 取待审批和授权状态。有没自动弹过的待审批就打开审批页：同一条只自动弹一次，你退出来不会反复弹。 */
    private fun refresh() {
        lastRefresh = System.currentTimeMillis()
        if (!prefs.configured) {
            if (pending.isNotEmpty() || targets != null || targetsError != null) {
                pending = emptyList()
                targets = null
                targetsError = null
                renderPending()
                renderGrants()
            }
            return
        }
        if (refreshing) return
        refreshing = true
        val client = PushmeClient(prefs.url, prefs.token)
        io.execute {
            val p = runCatching { client.pending() }
            val t = runCatching { client.targets() }
            main.post {
                refreshing = false
                if (isDestroyed) return@post
                p.onSuccess {
                    offsetMs = if (it.now > 0) it.now * 1000 - System.currentTimeMillis() else 0
                    if (it.approvals.map { a -> a.id } != pending.map { a -> a.id }) {
                        pending = it.approvals
                        renderPending()
                    }
                    autoOpen()
                }
                t.onSuccess {
                    if (it.targets != targets || targetsError != null) {
                        targets = it.targets
                        targetsError = null
                        renderGrants()
                    }
                }.onFailure {
                    val error = PushmeClient.describe(it)
                    if (error != targetsError) {
                        targetsError = error
                        renderGrants()
                    }
                }
            }
        }
    }

    private fun autoOpen() {
        val first = pending.firstOrNull { it.id !in autoOpened } ?: return
        if (isFinishing || !hasWindowFocus()) return
        autoOpened.add(first.id)
        startActivity(ApprovalActivity.intent(this, first.id))
    }

    private fun renderPending() {
        pendingBox.removeAllViews()
        pendingCountdowns.clear()
        if (pending.isEmpty()) return
        ui.add(pendingBox, ui.header("待审批 · ${pending.size}"))
        pending.forEachIndexed { i, a ->
            val left = ui.label(size = 13f, color = ui.warn, bold = true)
            pendingCountdowns.add(left to {
                val s = a.expires - serverNow()
                if (s <= 0) "已过期" else "还剩 %d:%02d".format(s / 60, s % 60)
            })
            ui.add(pendingBox, ui.card(
                ui.row(ui.label("来自 ${a.caller}", 13f, ui.muted), left, weights = listOf(1f, 0f)),
                ui.row(ui.label(a.title.ifBlank { "（无标题）" }, 17f, bold = true), ui.label("›", 24f, ui.muted), weights = listOf(1f, 0f)),
                gap = 4,
                stroke = ui.alpha(ui.warn, 0x99),
                onClick = { startActivity(ApprovalActivity.intent(this, a.id)) },
            ), top = if (i > 0) 8 else 0)
        }
        for ((view, text) in pendingCountdowns) view.text = text()
    }

    /** 当前授权：每个在线的 agent 一张卡片，列出生效中的授权和到期时间。 */
    private fun renderGrants() {
        grantBox.removeAllViews()
        grantCountdowns.clear()
        if (!prefs.configured) return
        ui.add(grantBox, ui.header("当前授权"))
        val list = targets
        val error = targetsError
        when {
            error != null -> ui.add(grantBox, ui.notice().also { ui.say(it, Tone.BAD, "取授权状态失败：$error") })
            list == null -> ui.add(grantBox, ui.card(ui.label("加载中…", 14f, ui.muted)))
            list.isEmpty() -> ui.add(grantBox, ui.card(ui.label("没有在线的 agent：宿主上的 pushme listen 没在跑，看不到授权状态，也下不了指令。", 14f, ui.muted)))
            else -> list.forEachIndexed { i, t -> ui.add(grantBox, grantCard(t), top = if (i > 0) 8 else 0) }
        }
        for ((view, text) in grantCountdowns) view.text = text()
    }

    private fun grantCard(t: CommandTarget): View {
        val views = mutableListOf<View>(ui.row(
            ui.label(t.label, 17f, bold = true),
            ui.label("开授权 / 收回 ›", 14f, ui.primary),
            weights = listOf(1f, 0f),
        ))
        val s = t.status
        val live = s?.grants.orEmpty().filter { it.expires > serverNow() }
        when {
            s == null -> views += ui.label("宿主上的 pushme listen 是旧版，没上报授权状态", 14f, ui.muted)
            live.isEmpty() -> views += ui.label("未授权", 14f, ui.muted)
            else -> for (g in live) {
                val left = ui.label(size = 13f, color = ui.warn, bold = true)
                grantCountdowns.add(left to { leftText(g.expires) })
                val detail = "到 ${timeText(g.expires)}" + if (g.by.isBlank()) "" else " · ${g.by}"
                views += ui.row(
                    ui.column(ui.label(GrantActivity.hostLabel(g.host), 15f, bold = true), ui.label(detail, 12f, ui.muted), gap = 2),
                    left,
                    weights = listOf(1f, 0f),
                )
            }
        }
        if (s != null && s.broken.isNotEmpty()) {
            views += ui.label("⚠ 源证书失效：${s.broken.joinToString("、") { GrantActivity.hostLabel(it) }}，授权了也用不了", 13f, ui.warn)
        }
        return ui.card(
            *views.toTypedArray(),
            gap = 10,
            stroke = if (live.isEmpty()) Color.TRANSPARENT else ui.alpha(ui.warn, 0x99),
            onClick = { startActivity(GrantActivity.intent(this, t.name)) },
        )
    }

    private fun leftText(expires: Long): String {
        val s = expires - serverNow()
        return when {
            s <= 0 -> "已到期"
            s < 60 -> "不到 1 分钟"
            s < 3600 -> "还剩 ${s / 60} 分钟"
            else -> "还剩 ${s / 3600} 小时 ${s % 3600 / 60} 分"
        }
    }

    companion object {
        private const val REFRESH_EVERY_MS = 5_000L

        /** 已经自动弹过的审批（进程内），见 [autoOpen]。 */
        private val autoOpened = HashSet<String>()
    }
}
