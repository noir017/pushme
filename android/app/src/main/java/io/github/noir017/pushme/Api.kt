package io.github.noir017.pushme

import org.json.JSONObject
import java.io.IOException
import java.net.HttpURLConnection
import java.net.SocketTimeoutException
import java.net.URL
import java.net.URLEncoder
import java.net.UnknownHostException

class ApiError(val code: Int, message: String) : IOException(message)

/**
 * pushme 的接收方客户端：收件箱（GET /inbox）与审批（GET /approvals、POST /approvals/{id}），
 * 契约见仓库根目录 README。用接收方 token 鉴权；同步阻塞，只在后台线程调用。
 */
class PushmeClient(private val url: String, private val token: String) {

    /**
     * 序号大于 [after] 的消息里最新的至多 [limit] 条；没有就最多挂 [waitSeconds] 秒。
     * [after] 为负时不回放历史，只返回收件箱当前的序号（第一次连上时用）。
     */
    fun inbox(after: Long, waitSeconds: Int, limit: Int): InboxBatch =
        InboxBatch.parse(request("GET", "/inbox?after=$after&wait=$waitSeconds&limit=$limit", null, (waitSeconds + 15) * 1000))

    /** 还在等答复的审批（含要签名的 challenge），以及服务器时间。 */
    fun pending(): PendingApprovals = PendingApprovals.parse(request("GET", "/approvals", null, 15_000))

    /** 批准（带签名）或拒绝（[sig] 为 null）。已答复过或已过期抛 [ApiError]（409）。 */
    fun answer(id: String, sig: String?) {
        val body = JSONObject()
            .put("decision", if (sig != null) "approve" else "deny")
            .apply { if (sig != null) put("sig", sig) }
        request("POST", "/approvals/" + URLEncoder.encode(id, "UTF-8"), body.toString(), 15_000)
    }

    /** 最近在线、能下指令的对象（宿主上跑着 pushme listen 的调用方）及其上报的授权状态，以及服务器时间。 */
    fun targets(): CommandTargets = CommandTargets.parse(request("GET", "/commands/targets", null, 15_000))

    /** 投递一条签好名的指令（字段与签名内容见 [Approver.commandPayload]）。 */
    fun sendCommand(c: SignedCommand) {
        val body = JSONObject()
            .put("to", c.to).put("id", c.id).put("issued", c.issued).put("expires", c.expires)
            .put("body", c.body).put("sig", c.sig)
        request("POST", "/commands", body.toString(), 15_000)
    }

    private fun request(method: String, path: String, body: String?, readTimeoutMs: Int): JSONObject {
        val conn = URL(url.trimEnd('/') + path).openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = 10_000
            conn.readTimeout = readTimeoutMs
            conn.setRequestProperty("Authorization", "Bearer $token")
            conn.setRequestProperty("Accept", "application/json")
            if (body != null) {
                conn.setRequestProperty("Content-Type", "application/json; charset=utf-8")
                conn.doOutput = true
                conn.outputStream.use { it.write(body.toByteArray(Charsets.UTF_8)) }
            }
            val code = conn.responseCode
            val stream = if (code in 200..299) conn.inputStream else conn.errorStream
            val text = stream?.bufferedReader(Charsets.UTF_8)?.use { it.readText() } ?: ""
            if (code !in 200..299) {
                val error = runCatching { JSONObject(text).optString("error") }.getOrNull()
                throw ApiError(code, if (error.isNullOrEmpty()) "HTTP $code" else error)
            }
            return JSONObject(text)
        } finally {
            conn.disconnect()
        }
    }

    companion object {
        fun describe(e: Throwable): String = when {
            e is ApiError && e.code == 401 -> "接收方 token 无效（pushme 上吊销了，或填错了）"
            e is ApiError && e.code == 404 -> "pushme 没开收件箱（PUSHME_RECEIVERS 为空），或审批不存在"
            e is ApiError && e.code == 409 -> "这条审批已经被处理或已过期"
            e is ApiError -> e.message ?: "HTTP ${e.code}"
            e is UnknownHostException -> "解析不了 pushme 的域名"
            e is SocketTimeoutException -> "连接 pushme 超时"
            e is IOException -> "连不上 pushme（${e.javaClass.simpleName}）"
            else -> e.message ?: e.javaClass.simpleName
        }
    }
}

/** 收件箱里的一条消息；[ts] 为受理时刻（epoch 秒），[title] 可能为空；待审批的带 [approvalId]。 */
data class InboxMessage(
    val seq: Long,
    val ts: Long,
    val caller: String,
    val title: String,
    val text: String,
    val approvalId: String?,
    val approvalExpires: Long,
)

/** `GET /inbox` 的响应：[skipped] 是因条数上限没返回的较早条数，[cursor] 是下次请求的 after。 */
data class InboxBatch(val messages: List<InboxMessage>, val skipped: Int, val cursor: Long) {
    companion object {
        fun parse(o: JSONObject): InboxBatch {
            val arr = o.optJSONArray("messages")
            val messages = (0 until (arr?.length() ?: 0)).map { i ->
                val m = arr!!.getJSONObject(i)
                val ap = m.optJSONObject("approval")
                InboxMessage(
                    m.getLong("seq"), m.optLong("ts"), m.optString("caller"), m.optString("title"), m.optString("text"),
                    ap?.optString("id")?.ifEmpty { null }, ap?.optLong("expires") ?: 0,
                )
            }
            return InboxBatch(messages, o.optInt("skipped"), o.getLong("cursor"))
        }
    }
}

/** 一条待审批。签名内容由 [Approver.payload] 按它拼出来，字段必须原样，不能 trim。 */
data class Approval(
    val id: String,
    val caller: String,
    val title: String,
    val text: String,
    val challenge: String,
    val created: Long,
    val expires: Long,
)

/** `GET /approvals`：[now] 是服务器时间（epoch 秒），用来换算本机时钟差。 */
data class PendingApprovals(val approvals: List<Approval>, val now: Long) {
    companion object {
        fun parse(o: JSONObject): PendingApprovals {
            val arr = o.optJSONArray("approvals")
            val list = (0 until (arr?.length() ?: 0)).map { i ->
                val a = arr!!.getJSONObject(i)
                Approval(
                    a.getString("id"), a.optString("caller"), a.optString("title"), a.optString("text"),
                    a.getString("challenge"), a.optLong("created"), a.getLong("expires"),
                )
            }
            return PendingApprovals(list, o.optLong("now"))
        }
    }
}

/** 能下指令的对象：[name] 是 pushme 调用方名（签进指令的收件方），[label] 是显示名（如 uniagent）；
 * [status] 是宿主上报的当前授权（旧版 pushme listen 不报，为 null）。 */
data class CommandTarget(val name: String, val label: String, val lastSeen: Long, val status: GrantStatus?)

/** 一条正在生效的授权：[host] 是机器名（all 为全部机器），[expires] 到期时刻（epoch 秒），[duration] 如 1h，[by] 谁开的。 */
data class Grant(val host: String, val expires: Long, val duration: String, val by: String)

/**
 * 宿主上报的授权状态（`agent-grant _state` 经 pushme listen 报上来，pushme 原样转发）：
 * [broken] 是源证书失效、授权了也用不了的机器；[at] 是上报时刻（epoch 秒）。
 */
data class GrantStatus(val grants: List<Grant>, val broken: List<String>, val at: Long) {
    companion object {
        fun parse(o: JSONObject, at: Long): GrantStatus {
            val g = o.optJSONArray("grants")
            val b = o.optJSONArray("broken")
            val grants = (0 until (g?.length() ?: 0)).mapNotNull { i ->
                g!!.optJSONObject(i)?.let { Grant(it.optString("host"), it.optLong("expires"), it.optString("duration"), it.optString("by")) }
            }
            return GrantStatus(grants, (0 until (b?.length() ?: 0)).map { b!!.optString(it) }, at)
        }
    }
}

/** `GET /commands/targets`：[now] 是服务器时间（epoch 秒），签发时刻按它校正。 */
data class CommandTargets(val targets: List<CommandTarget>, val now: Long) {
    companion object {
        fun parse(o: JSONObject): CommandTargets {
            val arr = o.optJSONArray("targets")
            val list = (0 until (arr?.length() ?: 0)).map { i ->
                val t = arr!!.getJSONObject(i)
                CommandTarget(
                    t.getString("name"), t.optString("label").ifEmpty { t.getString("name") }, t.optLong("last_seen"),
                    t.optJSONObject("status")?.let { GrantStatus.parse(it, t.optLong("status_at")) },
                )
            }
            return CommandTargets(list, o.optLong("now"))
        }
    }
}

/** 一条指令；[sig] 为空表示还没签。 */
data class SignedCommand(val to: String, val id: String, val issued: Long, val expires: Long, val body: String, val sig: String = "")
