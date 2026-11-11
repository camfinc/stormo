package stormo.wear.core

import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject

// The device API's answers (docs/devices.md), parsed with org.json. Parsers read only the fields
// the watch shows and ignore the rest, so the gateway can add fields without breaking the app.

data class PairStart(
    val deviceCode: String,
    val userCode: String,
    val fingerprint: String,
    val expiresIn: Int,
    val interval: Int,
)

data class Linked(val deviceId: String, val name: String)

data class Challenge(val nonce: String, val expiresIn: Int)

data class SessionToken(val token: String, val expiresIn: Int)

data class Grants(val status: Boolean, val review: Boolean, val chat: List<String>) {
    val chatsWithEveryAgent: Boolean get() = WILDCARD in chat
    fun canChat(agent: String): Boolean = chatsWithEveryAgent || agent in chat

    companion object {
        const val WILDCARD = "*"
        val NONE = Grants(status = false, review = false, chat = emptyList())
    }
}

data class Me(val deviceId: String, val name: String, val grants: Grants, val instance: String?)

data class FleetAgent(
    val id: String,
    val name: String,
    val unit: String?,
    val state: String,
    val busy: Boolean,
    val live: String?,
    val waiting: Int,
    val unread: Int,
    val pendingReview: Int,
)

data class Fleet(val agents: List<FleetAgent>, val polledAt: String?)

data class AiVerdict(val approve: Boolean, val reason: String?)

data class ReviewItem(
    val agent: String,
    val agentName: String,
    val kind: String,
    val id: String,
    /** Names exactly what was shown (16 hex digits); a decision carries it back. Absent on app-only items. */
    val version: String?,
    val summary: String,
    val ai: AiVerdict?,
    val appOnly: Boolean,
    val why: String?,
)

enum class Decision(val wire: String) { ACCEPT("accept"), REJECT("reject") }

data class ConversationSummary(
    val id: String,
    val title: String?,
    val preview: String?,
    val lastActive: String?,
    val messages: Int,
)

data class ChatMessage(val id: String, val role: String, val content: String, val at: String?) {
    val fromUser: Boolean get() = role == "user"
}

data class Conversation(val id: String, val compacted: Boolean, val messages: List<ChatMessage>)

data class TurnStarted(val turn: String, val conversation: String)

enum class TurnState { RUNNING, DONE, FAILED }

data class TurnStatus(
    val turn: String,
    val state: TurnState,
    val conversation: String?,
    val reply: String?,
    val error: String?,
)

internal object Parse {
    fun obj(body: String): JSONObject = try {
        JSONObject(body)
    } catch (e: JSONException) {
        throw ProtocolException("answer is not a JSON object", e)
    }

    fun pairStart(o: JSONObject) = PairStart(
        deviceCode = o.req("deviceCode"),
        userCode = o.req("userCode"),
        fingerprint = o.req("fingerprint"),
        expiresIn = o.optInt("expiresIn", 600),
        interval = o.optInt("interval", 5),
    )

    fun linked(o: JSONObject) = Linked(deviceId = o.req("deviceId"), name = o.stringOrNull("name") ?: "Watch")

    fun challenge(o: JSONObject) = Challenge(nonce = o.req("nonce"), expiresIn = o.optInt("expiresIn", 60))

    fun session(o: JSONObject) = SessionToken(token = o.req("token"), expiresIn = o.optInt("expiresIn", 3600))

    fun grants(o: JSONObject?): Grants {
        if (o == null) return Grants.NONE
        return Grants(
            status = o.optBoolean("status", false),
            review = o.optBoolean("review", false),
            chat = o.optJSONArray("chat").strings(),
        )
    }

    fun me(o: JSONObject): Me {
        val device = o.optJSONObject("device") ?: throw ProtocolException("me: no device")
        return Me(
            deviceId = device.req("id"),
            name = device.stringOrNull("name") ?: "Watch",
            grants = grants(device.optJSONObject("grants")),
            instance = o.stringOrNull("instance"),
        )
    }

    fun fleet(o: JSONObject) = Fleet(
        agents = o.optJSONArray("agents").objects().map { a ->
            FleetAgent(
                id = a.req("id"),
                name = a.stringOrNull("name") ?: a.req("id"),
                unit = a.stringOrNull("unit"),
                state = a.stringOrNull("state") ?: "unknown",
                busy = a.optBoolean("busy", false),
                live = a.stringOrNull("live"),
                waiting = a.optInt("waiting", 0),
                unread = a.optInt("unread", 0),
                pendingReview = a.optInt("pendingReview", 0),
            )
        },
        polledAt = o.stringOrNull("polledAt"),
    )

    fun reviewItems(o: JSONObject): List<ReviewItem> = o.optJSONArray("items").objects().map { i ->
        val ai = i.optJSONObject("ai")
        ReviewItem(
            agent = i.req("agent"),
            agentName = i.stringOrNull("agentName") ?: i.req("agent"),
            kind = i.stringOrNull("kind") ?: "",
            id = i.req("id"),
            version = i.stringOrNull("version")?.takeIf { it.isNotEmpty() },
            summary = i.stringOrNull("summary") ?: "",
            ai = ai?.let { AiVerdict(approve = it.optBoolean("approve", false), reason = it.stringOrNull("reason")) },
            // An item that does not say is treated as app-only: the watch never decides by default.
            appOnly = i.optBoolean("appOnly", true),
            why = i.stringOrNull("why"),
        )
    }

    fun conversations(o: JSONObject): List<ConversationSummary> =
        o.optJSONArray("conversations").objects().map { c ->
            ConversationSummary(
                id = c.req("id"),
                title = c.stringOrNull("title"),
                preview = c.stringOrNull("preview"),
                lastActive = c.stringOrNull("lastActive"),
                messages = c.optInt("messages", 0),
            )
        }

    fun conversation(o: JSONObject) = Conversation(
        id = o.req("id"),
        compacted = o.optBoolean("compacted", false),
        messages = o.optJSONArray("messages").objects().map { m ->
            ChatMessage(
                id = m.stringOrNull("id") ?: "",
                role = m.stringOrNull("role") ?: "assistant",
                content = m.stringOrNull("content") ?: "",
                at = m.stringOrNull("at"),
            )
        },
    )

    fun turnStarted(o: JSONObject) = TurnStarted(turn = o.req("turn"), conversation = o.req("conversation"))

    fun turnStatus(o: JSONObject) = TurnStatus(
        turn = o.req("turn"),
        state = when (o.stringOrNull("state")) {
            "done" -> TurnState.DONE
            "failed" -> TurnState.FAILED
            "running" -> TurnState.RUNNING
            else -> throw ProtocolException("turn: unknown state")
        },
        conversation = o.stringOrNull("conversation"),
        reply = o.stringOrNull("reply"),
        error = o.stringOrNull("error"),
    )
}

/** A string member, or null when it is absent or JSON null. */
internal fun JSONObject.stringOrNull(key: String): String? =
    if (!has(key) || isNull(key)) null else opt(key)?.toString()

internal fun JSONObject.req(key: String): String =
    stringOrNull(key)?.takeIf { it.isNotEmpty() } ?: throw ProtocolException("missing \"$key\"")

private fun JSONArray?.objects(): List<JSONObject> {
    if (this == null) return emptyList()
    return (0 until length()).mapNotNull { optJSONObject(it) }
}

private fun JSONArray?.strings(): List<String> {
    if (this == null) return emptyList()
    return (0 until length()).mapNotNull { if (isNull(it)) null else opt(it)?.toString() }
}
