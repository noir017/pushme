package io.github.noir017.pushme

import android.app.Activity
import android.content.res.ColorStateList
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.Drawable
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.RippleDrawable
import android.graphics.drawable.StateListDrawable
import android.text.InputType
import android.text.TextUtils
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.view.WindowInsets
import android.view.WindowManager
import android.widget.EditText
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView

/** 提示条、状态点的语气。 */
enum class Tone { PLAIN, INFO, GOOD, WARN, BAD }

/** 按钮样式：实心（主操作）、浅底、描边、纯文字。 */
enum class Btn { FILLED, TONAL, OUTLINE, TEXT }

/** 可点与否：不可点的变淡。 */
fun View.enable(on: Boolean) {
    isEnabled = on
    alpha = if (on) 1f else 0.4f
}

private const val MATCH = ViewGroup.LayoutParams.MATCH_PARENT
private const val WRAP = ViewGroup.LayoutParams.WRAP_CONTENT

/**
 * 界面的配色和常用控件。不依赖 AndroidX，全在代码里搭；颜色在 res/values(-night)/colors.xml，跟随系统深浅色。
 * 每页的骨架由 [screen] 搭：顶栏、可滚动的正文、可选的贴底操作栏。
 */
class Ui(private val activity: Activity) {
    private val density = activity.resources.displayMetrics.density
    fun dp(value: Int) = (value * density).toInt()

    val bg = activity.getColor(R.color.bg)
    val surface = activity.getColor(R.color.surface)
    val surface2 = activity.getColor(R.color.surface2)
    val line = activity.getColor(R.color.line)
    val ink = activity.getColor(R.color.text)
    val muted = activity.getColor(R.color.text2)
    val primary = activity.getColor(R.color.primary)
    val good = activity.getColor(R.color.good)
    val bad = activity.getColor(R.color.bad)
    val warn = activity.getColor(R.color.warn)

    fun color(tone: Tone) = when (tone) {
        Tone.PLAIN -> muted
        Tone.INFO -> primary
        Tone.GOOD -> good
        Tone.WARN -> warn
        Tone.BAD -> bad
    }

    /** 收件箱连接状态显示成什么语气。 */
    fun linkTone(link: PushService.Link) = when (link) {
        PushService.Link.ONLINE -> Tone.GOOD
        PushService.Link.CONNECTING -> Tone.WARN
        PushService.Link.ERROR -> Tone.BAD
        PushService.Link.OFF -> Tone.PLAIN
    }

    /** [color] 换成透明度 [a]（0～255）。 */
    fun alpha(color: Int, a: Int) = (color and 0x00FFFFFF) or (a shl 24)

    /**
     * 搭一页并设为内容：顶栏（[back] 时左侧有返回键，[action] 放右侧）、可滚动的 [body]、贴底的 [bottom]（可为空）。
     * 内容画到系统栏底下，按状态栏、导航栏和键盘的高度留边。
     *
     * [secure]：审批类页面用。不进截屏、录屏和最近任务的缩略图；被别的 App 的悬浮窗盖住时丢掉点击，
     * 防止有人用透明悬浮窗骗你点「批准」——加在根上，贴底的按钮也算在内。
     */
    fun screen(title: String, body: View, back: Boolean = true, action: View? = null, bottom: View? = null, secure: Boolean = false) {
        if (secure) activity.window.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
        // Android 15 起（targetSdk 35）系统强制这样，这一句只对 11～14 有用
        activity.window.setDecorFitsSystemWindows(false)
        val bar = LinearLayout(activity).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            minimumHeight = dp(56)
            setPadding(dp(if (back) 4 else 20), 0, dp(8), 0)
            if (back) addView(ImageView(activity).apply {
                setImageResource(R.drawable.ic_back)
                imageTintList = ColorStateList.valueOf(ink)
                scaleType = ImageView.ScaleType.CENTER
                contentDescription = "返回"
                background = RippleDrawable(ColorStateList.valueOf(alpha(ink, 0x1F)), null, null)
                setOnClickListener { activity.finish() }
            }, LinearLayout.LayoutParams(dp(48), dp(48)))
            addView(label(title, 20f, bold = true).apply { setPadding(dp(if (back) 8 else 0), 0, 0, 0) },
                LinearLayout.LayoutParams(0, WRAP, 1f))
            if (action != null) addView(action)
        }
        body.setPadding(dp(16), dp(4), dp(16), dp(24))
        val scroll = ScrollView(activity).apply {
            isFillViewport = true
            isVerticalScrollBarEnabled = false
            addView(body)
        }
        val root = LinearLayout(activity).apply {
            orientation = LinearLayout.VERTICAL
            setBackgroundColor(bg)
            filterTouchesWhenObscured = secure
            addView(bar, MATCH, WRAP)
            addView(scroll, LinearLayout.LayoutParams(MATCH, 0, 1f))
            if (bottom != null) addView(bottom, MATCH, WRAP)
        }
        root.setOnApplyWindowInsetsListener { v, insets ->
            val bars = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout())
            val ime = insets.getInsets(WindowInsets.Type.ime())
            v.setPadding(bars.left, bars.top, bars.right, maxOf(bars.bottom, ime.bottom))
            WindowInsets.CONSUMED
        }
        activity.setContentView(root)
    }

    /** 贴底的操作栏：顶上一条细线，[views] 竖着排。 */
    fun bottomBar(vararg views: View) = LinearLayout(activity).apply {
        orientation = LinearLayout.VERTICAL
        addView(View(activity).apply { setBackgroundColor(line) }, MATCH, 1)
        addView(column(*views, gap = 10).apply { setPadding(dp(16), dp(12), dp(16), dp(12)) }, MATCH, WRAP)
    }

    /** 竖排，子控件占满宽度，相邻之间隔 [gap] dp。 */
    fun column(vararg views: View, gap: Int = 0) = LinearLayout(activity).apply {
        orientation = LinearLayout.VERTICAL
        views.forEachIndexed { i, v -> addView(v, LinearLayout.LayoutParams(MATCH, WRAP).apply { if (i > 0) topMargin = dp(gap) }) }
    }

    /** 往竖排里追加一个占满宽度的控件，上方留 [top] dp。 */
    fun add(parent: LinearLayout, v: View, top: Int = 0) =
        parent.addView(v, LinearLayout.LayoutParams(MATCH, WRAP).apply { topMargin = dp(top) })

    /** 横排，按 [weights] 分宽（默认等分；0 表示按内容宽）。 */
    fun row(vararg views: View, weights: List<Float> = views.map { 1f }, gap: Int = 10) = LinearLayout(activity).apply {
        orientation = LinearLayout.HORIZONTAL
        gravity = Gravity.CENTER_VERTICAL
        views.forEachIndexed { i, v ->
            val w = weights[i]
            addView(v, LinearLayout.LayoutParams(if (w > 0) 0 else WRAP, WRAP, w).apply { if (i > 0) marginStart = dp(gap) })
        }
    }

    /** 按每行 [columns] 个等宽排开，最后一行不满用空位补齐。 */
    fun grid(views: List<View>, columns: Int): LinearLayout {
        val rows = views.chunked(columns).map { cells -> row(*Array(columns) { cells.getOrNull(it) ?: View(activity) }, gap = 8) }
        return column(*rows.toTypedArray(), gap = 8)
    }

    fun label(s: CharSequence = "", size: Float = 15f, color: Int = ink, bold: Boolean = false) = TextView(activity).apply {
        text = s
        textSize = size
        setTextColor(color)
        if (bold) typeface = Typeface.DEFAULT_BOLD
        setLineSpacing(0f, 1.15f)
    }

    /** 分组小标题。 */
    fun header(s: String) = label(s, 13f, muted, bold = true).apply { setPadding(dp(4), dp(20), dp(4), dp(8)) }

    /** 圆角卡片（竖排，间隔 [gap] dp）；给了 [onClick] 就整张可点，[stroke] 不透明时描边。 */
    fun card(vararg views: View, gap: Int = 0, stroke: Int = Color.TRANSPARENT, onClick: (() -> Unit)? = null) =
        column(*views, gap = gap).apply {
            setPadding(dp(16), dp(14), dp(16), dp(14))
            val shape = rounded(surface, 16, stroke)
            background = if (onClick == null) shape else pressable(shape, 16)
            if (onClick != null) setOnClickListener { onClick() }
        }

    /** 条目列表（[item]）放进一张卡片。 */
    fun list(vararg items: View) = column(*items).apply {
        setPadding(0, dp(4), 0, dp(4))
        background = rounded(surface, 16)
    }

    /** 列表里一行可点的条目：标题、说明（可空），右侧一段状态文字（如「已开启」「去开启 ›」）。返回整行和右侧文字。 */
    fun item(title: String, sub: String?, onClick: () -> Unit): Pair<View, TextView> {
        val end = label(size = 14f, color = muted)
        val texts = if (sub == null) column(label(title, 16f)) else column(label(title, 16f), label(sub, 13f, muted), gap = 2)
        val view = row(texts, end, weights = listOf(1f, 0f), gap = 12).apply {
            minimumHeight = dp(56)
            setPadding(dp(16), dp(10), dp(16), dp(10))
            background = pressable(rounded(Color.TRANSPARENT, 12), 12)
            setOnClickListener { onClick() }
        }
        return view to end
    }

    fun button(s: String, style: Btn = Btn.FILLED, tint: Int = primary, onClick: () -> Unit) = TextView(activity).apply {
        text = s
        textSize = 16f
        typeface = Typeface.DEFAULT_BOLD
        gravity = Gravity.CENTER
        minHeight = dp(if (style == Btn.TEXT) 44 else 52)
        setPadding(dp(16), dp(8), dp(16), dp(8))
        setTextColor(if (style == Btn.FILLED) Color.WHITE else tint)
        val fill = when (style) {
            Btn.FILLED -> tint
            Btn.TONAL -> alpha(tint, 0x22)
            else -> Color.TRANSPARENT
        }
        val stroke = if (style == Btn.OUTLINE) alpha(tint, 0x80) else Color.TRANSPARENT
        background = pressable(rounded(fill, 14, stroke), 14, if (style == Btn.FILLED) 0x33FFFFFF else alpha(tint, 0x29))
        setOnClickListener { onClick() }
    }

    /** 可选中的小块（选中与否看 [View.isSelected]），点了调 [onClick]。 */
    fun chip(s: String, onClick: () -> Unit) = TextView(activity).apply {
        text = s
        textSize = 15f
        gravity = Gravity.CENTER
        minHeight = dp(46)
        maxLines = 1
        ellipsize = TextUtils.TruncateAt.END
        setPadding(dp(8), dp(6), dp(8), dp(6))
        val selected = intArrayOf(android.R.attr.state_selected)
        setTextColor(ColorStateList(arrayOf(selected, intArrayOf()), intArrayOf(primary, ink)))
        background = pressable(StateListDrawable().apply {
            addState(selected, rounded(alpha(primary, 0x1F), 12, primary))
            addState(intArrayOf(), rounded(surface2, 12))
        }, 12)
        setOnClickListener { onClick() }
    }

    fun field(hint: String, value: String, password: Boolean = false) = EditText(activity).apply {
        this.hint = hint
        setText(value)
        textSize = 15f
        setTextColor(ink)
        setHintTextColor(alpha(muted, 0x99))
        inputType = InputType.TYPE_CLASS_TEXT or
            if (password) InputType.TYPE_TEXT_VARIATION_PASSWORD else InputType.TYPE_TEXT_VARIATION_URI
        background = rounded(surface2, 12)
        setPadding(dp(14), dp(12), dp(14), dp(12))
    }

    /** 提示条，用 [say] 设内容，内容为空就藏起来。 */
    fun notice() = label(size = 14f).apply {
        setPadding(dp(14), dp(10), dp(14), dp(10))
        visibility = View.GONE
    }

    fun say(notice: TextView, tone: Tone, s: String?) {
        notice.text = s
        notice.visibility = if (s.isNullOrEmpty()) View.GONE else View.VISIBLE
        notice.setTextColor(if (tone == Tone.PLAIN) ink else color(tone))
        notice.background = rounded(alpha(color(tone), 0x1C), 12)
    }

    /** 一行状态：圆点 + 文字，用 [set] 更新。 */
    inner class StatusLine {
        private val dot = View(activity).apply { background = GradientDrawable().apply { shape = GradientDrawable.OVAL } }
        private val words = label(size = 14f, color = muted)
        val view = LinearLayout(activity).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            addView(dot, LinearLayout.LayoutParams(dp(8), dp(8)).apply { marginEnd = dp(10) })
            addView(words, LinearLayout.LayoutParams(0, WRAP, 1f))
        }

        fun set(tone: Tone, s: String) {
            (dot.background as GradientDrawable).setColor(color(tone))
            words.text = s
        }
    }

    fun rounded(fill: Int, radius: Int, stroke: Int = Color.TRANSPARENT) = GradientDrawable().apply {
        setColor(fill)
        cornerRadius = dp(radius).toFloat()
        if (stroke != Color.TRANSPARENT) setStroke(dp(1), stroke)
    }

    /** 按下有水波纹。 */
    private fun pressable(content: Drawable, radius: Int, ripple: Int = alpha(ink, 0x1F)) =
        RippleDrawable(ColorStateList.valueOf(ripple), content, rounded(Color.WHITE, radius))
}
