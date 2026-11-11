package stormo.wear.tile

import android.content.Context
import android.text.format.DateFormat
import androidx.concurrent.futures.CallbackToFutureAdapter
import androidx.wear.protolayout.ActionBuilders
import androidx.wear.protolayout.ColorBuilders.argb
import androidx.wear.protolayout.DimensionBuilders.dp
import androidx.wear.protolayout.DimensionBuilders.expand
import androidx.wear.protolayout.DimensionBuilders.sp
import androidx.wear.protolayout.LayoutElementBuilders
import androidx.wear.protolayout.ModifiersBuilders
import androidx.wear.protolayout.ResourceBuilders
import androidx.wear.protolayout.TimelineBuilders
import androidx.wear.tiles.RequestBuilders
import androidx.wear.tiles.TileBuilders
import androidx.wear.tiles.TileService
import com.google.common.util.concurrent.ListenableFuture
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull
import stormo.wear.MainActivity
import stormo.wear.StormoApp
import stormo.wear.core.FleetSummary
import stormo.wear.data.LinkState
import java.util.Date
import java.util.concurrent.Executor

/**
 * The fleet at a glance: agents running, approvals waiting, proposals to review. It shows counts
 * only (never names or messages), refreshes them when they are older than [STALE_MILLIS] and the
 * watch can open a session, and opens the app when tapped.
 */
class FleetTileService : TileService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)

    override fun onTileRequest(requestParams: RequestBuilders.TileRequest): ListenableFuture<TileBuilders.Tile> =
        CallbackToFutureAdapter.getFuture { completer ->
            val job = scope.launch {
                val gateway = (application as StormoApp).gateway
                val cached = gateway.store.summary()
                val stale = cached == null || System.currentTimeMillis() - cached.second > STALE_MILLIS
                if (gateway.link.value is LinkState.Linked && stale) {
                    try {
                        withTimeoutOrNull(REFRESH_TIMEOUT_MILLIS) { gateway.fleet(notifyTile = false) }
                    } catch (e: CancellationException) {
                        throw e
                    } catch (_: Exception) {
                        // Offline, locked or refused: show the last counts.
                    }
                }
                completer.set(FleetTile.tile(this@FleetTileService, gateway.link.value, gateway.store.summary()))
            }
            completer.addCancellationListener({ job.cancel() }, Executor { it.run() })
            "fleet tile"
        }

    override fun onTileResourcesRequest(
        requestParams: RequestBuilders.ResourcesRequest,
    ): ListenableFuture<ResourceBuilders.Resources> =
        CallbackToFutureAdapter.getFuture { completer ->
            completer.set(ResourceBuilders.Resources.Builder().setVersion(FleetTile.RESOURCES_VERSION).build())
            "fleet tile resources"
        }

    override fun onDestroy() {
        scope.cancel()
        super.onDestroy()
    }

    companion object {
        const val STALE_MILLIS = 5 * 60_000L
        const val REFRESH_TIMEOUT_MILLIS = 10_000L
    }
}

/** The tile's layout, from the stored counts. */
object FleetTile {
    const val RESOURCES_VERSION = "1"
    private const val FRESHNESS_MILLIS = 15 * 60_000L
    private const val WHITE = 0xFFFFFFFF.toInt()
    private const val MUTED = 0xFFB0BEC5.toInt()
    private const val ACCENT = 0xFF8AB4F8.toInt()

    fun tile(context: Context, link: LinkState, summary: Pair<FleetSummary, Long>?): TileBuilders.Tile =
        TileBuilders.Tile.Builder()
            .setResourcesVersion(RESOURCES_VERSION)
            .setFreshnessIntervalMillis(FRESHNESS_MILLIS)
            .setTileTimeline(TimelineBuilders.Timeline.fromLayoutElement(layout(context, link, summary)))
            .build()

    private fun layout(context: Context, link: LinkState, summary: Pair<FleetSummary, Long>?): LayoutElementBuilders.LayoutElement {
        val column = LayoutElementBuilders.Column.Builder()
            .setHorizontalAlignment(LayoutElementBuilders.HORIZONTAL_ALIGN_CENTER)
            .addContent(text("Stormo", 14f, ACCENT, bold = true))
            .addContent(space())
        when {
            link !is LinkState.Linked -> column.addContent(text("Open to pair this watch", 16f, WHITE))
            summary == null -> column.addContent(text("Open to load the fleet", 16f, WHITE))
            else -> {
                val (s, at) = summary
                column.addContent(text("${s.running} of ${s.agents} running", 20f, WHITE, bold = true))
                    .addContent(space())
                    .addContent(text("${s.waiting} waiting", 15f, WHITE))
                    .addContent(text("${s.pendingReview} to review", 15f, WHITE))
                    .addContent(space())
                    .addContent(text("as of ${DateFormat.getTimeFormat(context).format(Date(at))}", 12f, MUTED))
            }
        }
        val open = ActionBuilders.LaunchAction.Builder()
            .setAndroidActivity(
                ActionBuilders.AndroidActivity.Builder()
                    .setPackageName(context.packageName)
                    .setClassName(MainActivity::class.java.name)
                    .build(),
            )
            .build()
        return LayoutElementBuilders.Box.Builder()
            .setWidth(expand())
            .setHeight(expand())
            .setModifiers(
                ModifiersBuilders.Modifiers.Builder()
                    .setClickable(ModifiersBuilders.Clickable.Builder().setId("open").setOnClick(open).build())
                    .build(),
            )
            .addContent(column.build())
            .build()
    }

    private fun text(value: String, size: Float, color: Int, bold: Boolean = false): LayoutElementBuilders.Text {
        val style = LayoutElementBuilders.FontStyle.Builder()
            .setSize(sp(size))
            .setColor(argb(color))
        if (bold) style.setWeight(LayoutElementBuilders.FONT_WEIGHT_BOLD)
        return LayoutElementBuilders.Text.Builder()
            .setText(value)
            .setFontStyle(style.build())
            .setMaxLines(2)
            .setMultilineAlignment(LayoutElementBuilders.TEXT_ALIGN_CENTER)
            .build()
    }

    private fun space() = LayoutElementBuilders.Spacer.Builder().setHeight(dp(4f)).build()
}
