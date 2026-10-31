package io.github.noir017.pushme

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** 开机、App 升级后把服务拉起来（填过配置才拉）。 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED && intent.action != Intent.ACTION_MY_PACKAGE_REPLACED) return
        if (!Prefs(context).configured) return
        try {
            PushService.start(context)
        } catch (e: RuntimeException) {
            // 个别 ROM 仍不许后台起前台服务：等用户打开一次 App
        }
    }
}
