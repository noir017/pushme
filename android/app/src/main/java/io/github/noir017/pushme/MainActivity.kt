package io.github.noir017.pushme

import android.Manifest
import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.TextUtils
import android.view.Gravity
import android.view.View
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.util.concurrent.Executors

/**
 * 主页：连接状态、待审批（点开去审批）、主动授权入口、最近的消息。地址、token、密钥、权限在 [SettingsActivity]。
 *
 * 在前台时每几秒查一次待审批和新消息；有没自动弹过的待审批就直接打开审批页（通知不一定弹得出来，进 App 就能处理）。
 * 待审批的正文只在审批页（禁截屏）里显示，主页和消息列表只露标题。
 */
class MainActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var status: Ui.StatusLine
    private lateinit var setupBox: LinearLayout
    private lateinit var pendingBox: LinearLayout
    private lateinit var grantEntry: View
    private lateinit var messageBox: LinearLayout

    private var pending: List<Approval> = emptyList()

    /** 待审批卡片上的倒计时，每秒刷新。 */
    private val countdowns = ArrayList<Pair<Approval, TextView>>()

    /** 服务器时间减本机时间（毫秒）。 */
    private var offsetMs = 0L

    /** 最近的消息，新的在前。[seenSeq] 是已取到的序号，[seenFrom] 是取它们时的地址和 token（换了收件箱就从头取）。 */
    private val messages = ArrayList<InboxMessage>()
    private var seenSeq = 0L
    private var seenFrom = ""
    private var messageError: String? = null
    private val expanded = HashSet<Long>()

    private var lastRefresh = 0L
    private var refreshing = false

    private val tick = object : Runnable {
        override fun run() {
            renderStatus()
            renderCountdowns()
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
        messageBox = ui.column(gap = 8)
        grantEntry = ui.card(
            ui.row(
                ui.column(ui.label("主动授权", 16f, bold = true), ui.label("不等申请，直接给 agent 开提权，或全部收回", 13f, ui.muted), gap = 2),
                ui.label("›", 24f, ui.muted),
                weights = listOf(1f, 0f),
            ),
            onClick = { startActivity(Intent(this, GrantActivity::class.java)) },
        )
        val body = ui.column(ui.card(status.view, onClick = { openSettings() }))
        ui.add(body, setupBox)
        ui.add(body, pendingBox)
        ui.add(body, grantEntry, top = 12)
        ui.add(body, ui.header("最近消息"))
        ui.add(body, messageBox)
        ui.screen("pushme", body, back = false, action = ui.button("设置", Btn.TEXT) { openSettings() })
        renderMessages()

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

    private fun renderStatus() {
        val link = PushService.link
        val host = runCatching { Uri.parse(prefs.url).host }.getOrNull()
        status.set(ui.linkTone(link), if (link == PushService.Link.ONLINE && host != null) "${PushService.status} · $host" else PushService.status)
    }

    /** 没配好时的引导：先连上 pushme，再生成密钥。 */
    private fun renderSetup() {
        setupBox.removeAllViews()
        grantEntry.visibility = if (prefs.configured) View.VISIBLE else View.GONE
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

    /** 取待审批和新消息。有没自动弹过的待审批就打开审批页：同一条只自动弹一次，你退出来不会反复弹。 */
    private fun refresh() {
        lastRefresh = System.currentTimeMillis()
        if (!prefs.configured) {
            if (seenFrom.isNotEmpty() || pending.isNotEmpty()) {
                pending = emptyList()
                messages.clear()
                seenFrom = ""
                messageError = null
                renderPending()
                renderMessages()
            }
            return
        }
        if (refreshing) return
        refreshing = true
        val client = PushmeClient(prefs.url, prefs.token)
        val from = prefs.url + " " + prefs.token
        val after = if (from == seenFrom) seenSeq else 0
        io.execute {
            val p = runCatching { client.pending() }
            val batch = runCatching { client.inbox(after, 0, HISTORY) }
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
                batch.onSuccess { b ->
                    // 换了收件箱，或者收件箱被清空过（序号倒退，服务器从头返回）
                    val reset = from != seenFrom || b.cursor < seenSeq
                    if (reset) {
                        messages.clear()
                        expanded.clear()
                    }
                    seenFrom = from
                    seenSeq = b.cursor
                    messages.addAll(0, b.messages.asReversed())
                    while (messages.size > HISTORY) messages.removeAt(messages.lastIndex)
                    if (reset || b.messages.isNotEmpty() || messageError != null) {
                        messageError = null
                        renderMessages()
                    }
                }.onFailure {
                    val error = PushmeClient.describe(it)
                    if (error != messageError) {
                        messageError = error
                        renderMessages()
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
        countdowns.clear()
        if (pending.isEmpty()) return
        ui.add(pendingBox, ui.header("待审批 · ${pending.size}"))
        pending.forEachIndexed { i, a ->
            val left = ui.label(size = 13f, color = ui.warn, bold = true)
            countdowns.add(a to left)
            ui.add(pendingBox, ui.card(
                ui.row(ui.label("来自 ${a.caller}", 13f, ui.muted), left, weights = listOf(1f, 0f)),
                ui.row(ui.label(a.title.ifBlank { "（无标题）" }, 17f, bold = true), ui.label("›", 24f, ui.muted), weights = listOf(1f, 0f)),
                gap = 4,
                stroke = ui.alpha(ui.warn, 0x99),
                onClick = { startActivity(ApprovalActivity.intent(this, a.id)) },
            ), top = if (i > 0) 8 else 0)
        }
        renderCountdowns()
    }

    private fun renderCountdowns() {
        for ((a, view) in countdowns) {
            val s = (a.expires * 1000 - offsetMs - System.currentTimeMillis()) / 1000
            view.text = if (s <= 0) "已过期" else "还剩 %d:%02d".format(s / 60, s % 60)
        }
    }

    private fun renderMessages() {
        messageBox.removeAllViews()
        val error = messageError
        if (error != null) {
            messageBox.addView(ui.notice().also { ui.say(it, Tone.BAD, "取消息失败：$error") })
        }
        if (messages.isEmpty()) {
            val note = when {
                !prefs.configured -> "连上 pushme 后，这里显示最近的消息"
                error != null -> return
                seenFrom.isEmpty() -> "加载中…"
                else -> "还没有消息"
            }
            messageBox.addView(ui.label(note, 14f, ui.muted).apply {
                gravity = Gravity.CENTER
                setPadding(0, ui.dp(32), 0, ui.dp(32))
            })
            return
        }
        for (m in messages) ui.add(messageBox, messageCard(m), top = if (messageBox.childCount > 0) 8 else 0)
    }

    /** 一条消息：调用方、时间、标题、正文（长的先折起来，点一下展开）；长按复制。待审批的只露标题。 */
    private fun messageCard(m: InboxMessage): View {
        val isApproval = m.approvalId != null
        val views = mutableListOf<View>(ui.row(
            ui.label(if (isApproval) "${m.caller} · 审批" else m.caller, 13f, if (isApproval) ui.warn else ui.muted, bold = true),
            ui.label(timeText(m.ts), 12f, ui.muted),
            weights = listOf(1f, 0f),
        ))
        if (m.title.isNotBlank()) views += ui.label(m.title, 16f, bold = true)
        val textView = if (isApproval || m.text.isBlank()) null else ui.label(m.text, 15f).apply {
            maxLines = if (m.seq in expanded) Int.MAX_VALUE else COLLAPSED_LINES
            ellipsize = TextUtils.TruncateAt.END
        }
        if (textView != null) views += textView
        return ui.card(*views.toTypedArray(), gap = 4, onClick = {
            if (textView != null) {
                val open = m.seq !in expanded
                if (open) expanded.add(m.seq) else expanded.remove(m.seq)
                textView.maxLines = if (open) Int.MAX_VALUE else COLLAPSED_LINES
            }
        }).apply {
            setOnLongClickListener {
                val copied = if (isApproval) m.title else listOf(m.title, m.text).filter { it.isNotBlank() }.joinToString("\n")
                getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("pushme 消息", copied))
                Toast.makeText(this@MainActivity, "已复制", Toast.LENGTH_SHORT).show()
                true
            }
        }
    }

    /** 今天的只显示时分，昨天的带「昨天」，更早的带日期。 */
    private fun timeText(ts: Long): String {
        if (ts <= 0) return ""
        val zone = ZoneId.systemDefault()
        val t = Instant.ofEpochSecond(ts).atZone(zone)
        val today = LocalDate.now(zone)
        val hm = t.format(HOUR_MINUTE)
        return when (val d = t.toLocalDate()) {
            today -> hm
            today.minusDays(1) -> "昨天 $hm"
            else -> if (d.year == today.year) "${d.monthValue}月${d.dayOfMonth}日 $hm" else "${d.year}-${d.monthValue}-${d.dayOfMonth}"
        }
    }

    companion object {
        private const val REFRESH_EVERY_MS = 5_000L
        // 主页最多显示的消息条数（收件箱只留最近 200 条）
        private const val HISTORY = 50
        private const val COLLAPSED_LINES = 5
        private val HOUR_MINUTE = DateTimeFormatter.ofPattern("HH:mm")

        /** 已经自动弹过的审批（进程内），见 [autoOpen]。 */
        private val autoOpened = HashSet<String>()
    }
}
