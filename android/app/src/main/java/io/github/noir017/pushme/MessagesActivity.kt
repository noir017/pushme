package io.github.noir017.pushme

import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.text.TextUtils
import android.view.Gravity
import android.view.View
import android.widget.LinearLayout
import android.widget.Toast
import java.util.concurrent.Executors

/**
 * 消息页：收件箱里最近的消息（`GET /inbox?after=0`，从服务器取，手机上不另存），新的在上。
 * 在前台时每几秒补收一次新的。长消息先折起来，点一下展开；长按复制。待审批的只露标题（正文只在禁截屏的审批页显示）。
 */
class MessagesActivity : Activity() {

    private val main = Handler(Looper.getMainLooper())
    private val io = Executors.newSingleThreadExecutor()
    private lateinit var prefs: Prefs
    private lateinit var ui: Ui
    private lateinit var messageBox: LinearLayout

    /** 最近的消息，新的在前。[seenSeq] 是已取到的序号，[seenFrom] 是取它们时的地址和 token（换了收件箱就从头取）。 */
    private val messages = ArrayList<InboxMessage>()
    private var seenSeq = 0L
    private var seenFrom = ""
    private var messageError: String? = null
    private val expanded = HashSet<Long>()
    private var refreshing = false

    private val tick = object : Runnable {
        override fun run() {
            refresh()
            main.postDelayed(this, REFRESH_EVERY_MS)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        ui = Ui(this)
        messageBox = ui.column()
        ui.screen("消息", ui.column(messageBox))
        render()
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

    private fun refresh() {
        if (!prefs.configured || refreshing) return
        refreshing = true
        val client = PushmeClient(prefs.url, prefs.token)
        val from = prefs.url + " " + prefs.token
        val after = if (from == seenFrom) seenSeq else 0
        io.execute {
            val batch = runCatching { client.inbox(after, 0, HISTORY) }
            main.post {
                refreshing = false
                if (isDestroyed) return@post
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
                        render()
                    }
                }.onFailure {
                    val error = PushmeClient.describe(it)
                    if (error != messageError) {
                        messageError = error
                        render()
                    }
                }
            }
        }
    }

    private fun render() {
        messageBox.removeAllViews()
        val error = messageError
        if (error != null) {
            ui.add(messageBox, ui.notice().also { ui.say(it, Tone.BAD, "取消息失败：$error") })
        }
        if (messages.isEmpty()) {
            val note = when {
                !prefs.configured -> "连上 pushme 后，这里显示最近的消息"
                error != null -> return
                seenFrom.isEmpty() -> "加载中…"
                else -> "还没有消息"
            }
            ui.add(messageBox, ui.label(note, 14f, ui.muted).apply {
                gravity = Gravity.CENTER
                setPadding(0, ui.dp(48), 0, ui.dp(48))
            })
            return
        }
        for (m in messages) ui.add(messageBox, messageCard(m), top = if (messageBox.childCount > 0) 8 else 0)
    }

    /** 一条消息：调用方、时间、标题、正文（长的先折起来，点一下展开）；长按复制。待审批的只露标题。 */
    private fun messageCard(m: InboxMessage): View {
        val isApproval = m.approvalId != null
        val tag = when {
            isApproval -> " · 审批"
            m.requestId != null -> " · 申请"
            else -> ""
        }
        val views = mutableListOf<View>(ui.row(
            ui.label(m.caller + tag, 13f, if (tag.isEmpty()) ui.muted else ui.warn, bold = true),
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
                Toast.makeText(this@MessagesActivity, "已复制", Toast.LENGTH_SHORT).show()
                true
            }
        }
    }

    companion object {
        private const val REFRESH_EVERY_MS = 5_000L
        // 最多显示的条数（收件箱只留最近 200 条）
        private const val HISTORY = 50
        private const val COLLAPSED_LINES = 5
    }
}
