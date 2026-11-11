package stormo.wear.core

import java.time.Instant
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeParseException
import java.time.format.FormatStyle
import java.util.Locale

/** What a review decision came to. */
sealed interface DecideResult {
    data object Done : DecideResult

    /** Decided in the app only: shown with the gateway's reason. */
    data class AppOnly(val why: String?) : DecideResult

    /** Gone: decided elsewhere meanwhile, or withdrawn. */
    data object Gone : DecideResult

    /** The proposal changed since it was shown: nothing was decided; reload the list. */
    data object Changed : DecideResult

    /** The review is busy (rate limited): nothing was decided; try again after [seconds]. */
    data class TryAgain(val seconds: Long?) : DecideResult
}

object Review {
    /**
     * Only items the gateway marks `appOnly: false`, with the version that names what was shown,
     * are decided on the watch.
     */
    fun decidableOnWatch(item: ReviewItem): Boolean = !item.appOnly && item.version != null

    /**
     * Sends a decision, refusing locally (no request at all) for an app-only item. The gateway's
     * `app_only`, `not_found`, `changed` and `rate_limited` (the core answers a contended review
     * with it) are results, not failures. `busy` belongs to chat and is not one of them.
     */
    suspend fun decide(item: ReviewItem, decision: Decision, post: suspend (ReviewItem, Decision) -> Unit): DecideResult {
        if (!decidableOnWatch(item)) return DecideResult.AppOnly(item.why)
        return try {
            post(item, decision)
            DecideResult.Done
        } catch (e: GatewayException) {
            when (e.code) {
                ErrorCode.APP_ONLY -> DecideResult.AppOnly(item.why ?: e.serverMessage)
                ErrorCode.NOT_FOUND -> DecideResult.Gone
                ErrorCode.CHANGED -> DecideResult.Changed
                ErrorCode.RATE_LIMITED -> DecideResult.TryAgain(e.retryAfterSeconds)
                else -> throw e
            }
        }
    }

    /** One line for the AI's verdict, or null when there is none yet. */
    fun verdictLine(item: ReviewItem): String? {
        val ai = item.ai ?: return null
        val head = if (ai.approve) "AI: approve" else "AI: don't approve"
        return if (ai.reason.isNullOrBlank()) head else "$head. ${ai.reason}"
    }
}

object Chat {
    const val POLL_MILLIS = 3_000L

    /** A turn runs at most 10 minutes; one more lets the gateway report it. */
    const val GIVE_UP_MILLIS = 11 * 60_000L
    const val MAX_TRANSIENT_FAILURES = 5

    /** Null when the message can be sent, else why not. */
    fun problem(message: String): String? = when {
        message.isBlank() -> "Say or type something first."
        message.length > GatewayClient.MAX_MESSAGE -> "That's too long (at most ${GatewayClient.MAX_MESSAGE} characters)."
        else -> null
    }

    /**
     * Polls a turn every [POLL_MILLIS] until it is done or failed. Short outages are ridden out; a
     * locked watch pauses the polling and a rate limit is waited out (neither counts toward giving
     * up); anything else (the turn is gone, the link is lost) is thrown.
     */
    suspend fun await(
        fetch: suspend () -> TurnStatus,
        sleep: suspend (Long) -> Unit,
        clock: () -> Long = System::currentTimeMillis,
    ): TurnStatus {
        var giveUp = clock() + GIVE_UP_MILLIS
        var failures = 0
        while (true) {
            sleep(POLL_MILLIS)
            try {
                val status = fetch()
                failures = 0
                if (status.state != TurnState.RUNNING) return status
            } catch (e: DeviceLockedException) {
                // Nothing is sent while locked; the turn keeps running and is read after unlock.
                giveUp += POLL_MILLIS
                continue
            } catch (e: GatewayException) {
                if (e.code == ErrorCode.RATE_LIMITED) {
                    val wait = (e.retryAfterSeconds ?: GatewayClient.DEFAULT_RETRY_AFTER_SECONDS) * 1000
                    sleep(wait)
                    giveUp += wait + POLL_MILLIS
                    continue
                }
                if (!e.isTransient() || ++failures >= MAX_TRANSIENT_FAILURES) throw e
            } catch (e: StormoException) {
                if (!e.isTransient() || ++failures >= MAX_TRANSIENT_FAILURES) throw e
            }
            if (clock() >= giveUp) throw ProtocolException("the reply took longer than a turn can run")
        }
    }
}

/** The fleet as the tile shows it: counts only, nothing anyone said. */
data class FleetSummary(val agents: Int, val running: Int, val busy: Int, val waiting: Int, val pendingReview: Int) {
    companion object {
        fun of(fleet: Fleet) = FleetSummary(
            agents = fleet.agents.size,
            running = fleet.agents.count { it.state == "running" },
            busy = fleet.agents.count { it.busy },
            waiting = fleet.agents.sumOf { it.waiting },
            pendingReview = fleet.agents.sumOf { it.pendingReview },
        )
    }
}

object AgentText {
    /** The second line of an agent's row: its state, what it runs now, and what waits for you. */
    fun status(agent: FleetAgent): String {
        val parts = ArrayList<String>()
        val state = agent.state.replaceFirstChar { it.titlecase(Locale.ROOT) }
        parts += if (agent.live != null) "$state · ${agent.live}" else state
        if (agent.unread > 0) parts += "${agent.unread} unread"
        if (agent.waiting > 0) parts += "${agent.waiting} waiting"
        if (agent.pendingReview > 0) parts += "${agent.pendingReview} to review"
        return parts.joinToString(" · ")
    }
}

/** Short labels for the gateway's RFC 3339 times. */
object TimeLabel {
    fun of(text: String?, now: Instant = Instant.now(), zone: ZoneId = ZoneId.systemDefault()): String? {
        if (text.isNullOrBlank()) return null
        val instant = try {
            OffsetDateTime.parse(text).toInstant()
        } catch (_: DateTimeParseException) {
            return null
        }
        val at = instant.atZone(zone)
        val today = now.atZone(zone).toLocalDate()
        val style = if (at.toLocalDate() == today) {
            DateTimeFormatter.ofLocalizedTime(FormatStyle.SHORT)
        } else {
            DateTimeFormatter.ofPattern("MMM d")
        }
        return at.format(style)
    }
}
