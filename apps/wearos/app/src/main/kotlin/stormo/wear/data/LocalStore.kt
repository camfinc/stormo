package stormo.wear.data

import android.content.Context
import stormo.wear.core.FleetSummary
import stormo.wear.core.Linked

/**
 * What the watch keeps on disk: the gateway's address, the link (device id and the name the owner
 * gave) and the tile's last counts. Never a session token, a device code or anything anyone said.
 * Excluded from backups and device transfer (res/xml/data_extraction_rules.xml).
 */
class LocalStore(context: Context) {
    private val prefs = context.getSharedPreferences(FILE, Context.MODE_PRIVATE)

    var gatewayUrl: String?
        get() = prefs.getString(GATEWAY_URL, null)?.takeIf { it.isNotEmpty() }
        set(value) {
            prefs.edit().putString(GATEWAY_URL, value).apply()
        }

    val linked: Linked?
        get() {
            val id = prefs.getString(DEVICE_ID, null) ?: return null
            return Linked(id, prefs.getString(DEVICE_NAME, null) ?: "Watch")
        }

    fun saveLink(linked: Linked) {
        prefs.edit().putString(DEVICE_ID, linked.deviceId).putString(DEVICE_NAME, linked.name).apply()
    }

    fun saveName(name: String) {
        prefs.edit().putString(DEVICE_NAME, name).apply()
    }

    /** Forgets the link and the tile's counts; the gateway address stays unless [everything]. */
    fun clear(everything: Boolean) {
        val edit = prefs.edit()
        if (everything) {
            edit.clear()
        } else {
            edit.remove(DEVICE_ID).remove(DEVICE_NAME)
            SUMMARY_KEYS.forEach { edit.remove(it) }
        }
        edit.apply()
    }

    fun saveSummary(summary: FleetSummary, at: Long) {
        prefs.edit()
            .putInt(S_AGENTS, summary.agents)
            .putInt(S_RUNNING, summary.running)
            .putInt(S_BUSY, summary.busy)
            .putInt(S_WAITING, summary.waiting)
            .putInt(S_REVIEW, summary.pendingReview)
            .putLong(S_AT, at)
            .apply()
    }

    /** The last counts and when they were read, or null before the first read. */
    fun summary(): Pair<FleetSummary, Long>? {
        val at = prefs.getLong(S_AT, 0)
        if (at == 0L) return null
        return FleetSummary(
            agents = prefs.getInt(S_AGENTS, 0),
            running = prefs.getInt(S_RUNNING, 0),
            busy = prefs.getInt(S_BUSY, 0),
            waiting = prefs.getInt(S_WAITING, 0),
            pendingReview = prefs.getInt(S_REVIEW, 0),
        ) to at
    }

    private companion object {
        const val FILE = "stormo"
        const val GATEWAY_URL = "gateway_url"
        const val DEVICE_ID = "device_id"
        const val DEVICE_NAME = "device_name"
        const val S_AGENTS = "summary_agents"
        const val S_RUNNING = "summary_running"
        const val S_BUSY = "summary_busy"
        const val S_WAITING = "summary_waiting"
        const val S_REVIEW = "summary_review"
        const val S_AT = "summary_at"
        val SUMMARY_KEYS = listOf(S_AGENTS, S_RUNNING, S_BUSY, S_WAITING, S_REVIEW, S_AT)
    }
}
