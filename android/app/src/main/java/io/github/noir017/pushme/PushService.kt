package io.github.noir017.pushme

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.IBinder

/**
 * 前台服务：长轮询 pushme 的收件箱，来消息就弹通知；遇到待审批，按 `GET /approvals` 的内容弹审批通知，
 * 点开进 [ApprovalActivity]。每次（重新）连上收件箱也查一遍待审批，断线期间来的照样能看到。
 *
 * 谁会启动它：主页（填过配置时）、设置页「保存并连接」、开机和 App 升级后的 [BootReceiver]。
 */
class PushService : Service() {

    private lateinit var prefs: Prefs
    private var config: Pair<String, String>? = null

    /** 轮询线程的代号：配置变了或服务销毁就 +1，旧线程看到对不上就自行退出。 */
    @Volatile
    private var generation = 0

    /** 已经弹过通知的审批，免得每次重连都再弹一遍。只在轮询线程里用。 */
    private val notified = HashSet<String>()

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        prefs = Prefs(this)
        getSystemService(NotificationManager::class.java).createNotificationChannels(
            listOf(
                NotificationChannel(CHANNEL_STATUS, "运行状态", NotificationManager.IMPORTANCE_LOW),
                NotificationChannel(CHANNEL_MESSAGES, "消息", NotificationManager.IMPORTANCE_HIGH).apply {
                    description = "pushme 推来的消息：弹横幅、响铃"
                },
                NotificationChannel(CHANNEL_MESSAGES_QUIET, "消息（静默）", NotificationManager.IMPORTANCE_LOW).apply {
                    description = "离线后一次补收多条时，较早的那些：只进通知栏"
                },
                NotificationChannel(CHANNEL_APPROVALS, "审批", NotificationManager.IMPORTANCE_HIGH).apply {
                    description = "需要你确认的操作（如 agent 申请提权）。锁屏上不显示内容"
                    lockscreenVisibility = Notification.VISIBILITY_PRIVATE
                },
            )
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // startForegroundService 拉起的必须几秒内 startForeground，先挂上通知
        startForeground(NOTIFICATION_ID, statusNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        if (!prefs.configured) {
            setStatus(Link.OFF, "未配置：在设置里填 pushme 地址和接收方 token")
            return START_STICKY
        }
        val c = prefs.url to prefs.token
        if (c != config) {
            config = c
            startPolling(c)
        }
        return START_STICKY
    }

    override fun onDestroy() {
        generation++
        config = null
        status = "已停止"
        link = Link.OFF
        super.onDestroy()
    }

    private fun startPolling(c: Pair<String, String>) {
        val gen = ++generation
        val client = PushmeClient(c.first, c.second)
        setStatus(Link.CONNECTING, "连接中…")
        Thread({
            var backoff = MIN_BACKOFF
            var connected: Boolean? = null
            while (gen == generation) {
                try {
                    val cursor = prefs.cursor
                    val batch = client.inbox(cursor, if (cursor < 0) 0 else POLL_SECONDS, LIMIT)
                    if (gen != generation) break
                    backoff = MIN_BACKOFF
                    if (connected != true) {
                        connected = true
                        setStatus(Link.ONLINE, "已连上")
                        syncApprovals(client) // 断线期间来的审批
                    }
                    if (cursor >= 0) {
                        notifyMessages(batch)
                        if (batch.messages.any { it.approvalId != null }) syncApprovals(client)
                    }
                    prefs.cursor = batch.cursor
                } catch (e: Exception) {
                    if (gen != generation) break
                    connected = false
                    setStatus(Link.ERROR, "连不上：${PushmeClient.describe(e)}")
                    try {
                        Thread.sleep(backoff)
                    } catch (_: InterruptedException) {
                        break
                    }
                    backoff = (backoff * 2).coerceAtMost(MAX_BACKOFF)
                }
            }
        }, "pushme-poll").apply { isDaemon = true }.start()
    }

    /**
     * 普通消息一条一个通知；离线后重连一次补收好几条时，只让最新一条提醒，其余静默。
     * 待审批的消息不在这里弹：内容以 `GET /approvals` 为准（[syncApprovals]）。
     */
    private fun notifyMessages(batch: InboxBatch) {
        val msgs = batch.messages.filter { it.approvalId == null }
        val nm = getSystemService(NotificationManager::class.java)
        if (batch.skipped > 0) {
            nm.notify(SKIPPED_NOTIFICATION_ID, messageNotification("pushme", "还有 ${batch.skipped} 条较早的消息没显示", "离线太久，完整记录在飞书。", 0, false))
        }
        msgs.forEachIndexed { i, m ->
            val alert = msgs.size <= BURST_ALERTS || i == msgs.lastIndex
            nm.notify(MESSAGE_ID_BASE + (m.seq % MESSAGE_ID_SPAN).toInt(), messageNotification(m.caller, m.title, m.text, m.ts * 1000, alert))
            if (i < msgs.lastIndex) Thread.sleep(NOTIFY_GAP_MS) // 一口气连发，系统会丢掉一部分
        }
    }

    private fun messageNotification(caller: String, title: String, text: String, whenMs: Long, alert: Boolean): Notification {
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        return Notification.Builder(this, if (alert) CHANNEL_MESSAGES else CHANNEL_MESSAGES_QUIET)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(if (title.isEmpty()) "[$caller]" else "[$caller] $title")
            .setContentText(text)
            .setStyle(Notification.BigTextStyle().bigText(text))
            .setCategory(Notification.CATEGORY_MESSAGE)
            .setWhen(if (whenMs > 0) whenMs else System.currentTimeMillis())
            .setShowWhen(true)
            .setAutoCancel(true)
            .setContentIntent(open)
            .build()
    }

    /** 查一遍待审批，没弹过的弹通知。查不到只记状态，下次来消息或重连时再查。 */
    private fun syncApprovals(client: PushmeClient) {
        val pending = try {
            client.pending()
        } catch (e: Exception) {
            setStatus(Link.ONLINE, "已连上（查待审批失败：${PushmeClient.describe(e)}）")
            return
        }
        val offsetMs = if (pending.now > 0) pending.now * 1000 - System.currentTimeMillis() else 0
        for (a in pending.approvals) {
            if (notified.add(a.id)) notifyApproval(a, offsetMs)
        }
    }

    private fun notifyApproval(a: Approval, offsetMs: Long) {
        val leftMs = a.expires * 1000 - offsetMs - System.currentTimeMillis()
        if (leftMs <= 0) return
        val open = PendingIntent.getActivity(
            this, a.id.hashCode(), ApprovalActivity.intent(this, a.id),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        // 锁屏上只显示这一版：不露内容，点了先解锁
        val public = Notification.Builder(this, CHANNEL_APPROVALS)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle("有一条待审批")
            .setContentText("解锁手机后查看")
            .build()
        val n = Notification.Builder(this, CHANNEL_APPROVALS)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle("待审批：" + a.title.ifBlank { a.caller })
            .setContentText(a.text)
            .setSubText(a.caller)
            .setStyle(Notification.BigTextStyle().bigText(a.text + "\n\n点开审批，${(leftMs + 59_999) / 60_000} 分钟内有效"))
            .setCategory(Notification.CATEGORY_REMINDER)
            .setVisibility(Notification.VISIBILITY_PRIVATE)
            .setPublicVersion(public)
            .setTimeoutAfter(leftMs) // 过期自动收起
            .setAutoCancel(true)
            .setContentIntent(open)
            .build()
        getSystemService(NotificationManager::class.java).notify(approvalNotificationId(a.id), n)
    }

    private fun setStatus(state: Link, text: String) {
        link = state
        status = text
        getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, statusNotification())
    }

    private fun statusNotification(): Notification {
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        return Notification.Builder(this, CHANNEL_STATUS)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(status)
            .setOngoing(true)
            .setContentIntent(open)
            .build()
    }

    /** 收件箱连接状态：没在跑、连接中、连上了、连不上（在退避重试）。 */
    enum class Link { OFF, CONNECTING, ONLINE, ERROR }

    companion object {
        private const val CHANNEL_STATUS = "status"
        private const val CHANNEL_MESSAGES = "messages"
        private const val CHANNEL_MESSAGES_QUIET = "messages_quiet"
        private const val CHANNEL_APPROVALS = "approvals"
        private const val NOTIFICATION_ID = 1
        private const val SKIPPED_NOTIFICATION_ID = 2
        private const val APPROVAL_ID_BASE = 500_000
        private const val MESSAGE_ID_BASE = 1_000
        private const val MESSAGE_ID_SPAN = 100_000L

        // pushme 单次最多挂 60 秒，取 50 秒少醒几次
        private const val POLL_SECONDS = 50
        private const val LIMIT = 20
        // 一次收到不超过这么多条就逐条提醒，超过只提醒最新一条
        private const val BURST_ALERTS = 3
        private const val NOTIFY_GAP_MS = 300L
        private const val MIN_BACKOFF = 2_000L
        private const val MAX_BACKOFF = 30_000L

        /** 最近一次状态文字和连接状态，界面展示用。 */
        @Volatile
        var status: String = "未启动"
            private set

        @Volatile
        var link: Link = Link.OFF
            private set

        fun start(context: Context) {
            context.startForegroundService(Intent(context, PushService::class.java))
        }

        fun approvalNotificationId(id: String) = APPROVAL_ID_BASE + (id.hashCode() and 0xffff)

        fun cancelApproval(context: Context, id: String) {
            context.getSystemService(NotificationManager::class.java).cancel(approvalNotificationId(id))
        }
    }
}
