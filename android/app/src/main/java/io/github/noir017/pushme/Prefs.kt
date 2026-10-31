package io.github.noir017.pushme

import android.content.Context

/** pushme 的地址、接收方 token（在主界面上填），以及收件箱读到哪了。 */
class Prefs(context: Context) {
    private val sp = context.getSharedPreferences("pushme", Context.MODE_PRIVATE)

    var url: String
        get() = sp.getString("url", "") ?: ""
        set(value) = sp.edit().putString("url", value).apply()

    var token: String
        get() = sp.getString("token", "") ?: ""
        set(value) = sp.edit().putString("token", value).apply()

    /** 收件箱里已收到的最新序号；-1 表示还没连上过这个收件箱（第一次连上只记下序号，不回放历史）。 */
    var cursor: Long
        get() = sp.getLong("cursor", -1)
        set(value) = sp.edit().putLong("cursor", value).apply()

    val configured: Boolean
        get() = url.isNotEmpty() && token.isNotEmpty()
}
