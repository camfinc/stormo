package stormo.wear.core

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.net.URLEncoder
import kotlin.coroutines.CoroutineContext

/**
 * The device API (`/device/v1/…`), one method per route, typed in and out. Errors are thrown as
 * [GatewayException] (a contract code), [NetworkException] or [ProtocolException]. A 429 is retried
 * once after its `Retry-After` when that is short; a longer one is thrown for the caller to show.
 * Nothing here logs: bodies carry codes, tokens and messages.
 */
class GatewayClient(
    baseUrl: String,
    private val transport: Transport,
    private val io: CoroutineContext = Dispatchers.IO,
    private val sleep: suspend (Long) -> Unit = { delay(it) },
) {
    val baseUrl: String = baseUrl.trimEnd('/')

    suspend fun health(): Boolean = call("GET", "/health").optString("status") == "ok"

    suspend fun pair(info: DeviceInfo, key: EcPublicJwk, attestation: List<String>): PairStart {
        val body = JSONObject()
            .put("name", info.name)
            .put("model", info.model)
            .put("os", info.os)
            .put("appVersion", info.appVersion)
            .put(
                "publicKey",
                JSONObject().put("kty", EcPublicJwk.KTY).put("crv", EcPublicJwk.CRV).put("x", key.x).put("y", key.y),
            )
        if (attestation.isNotEmpty()) body.put("attestation", JSONArray(attestation))
        return Parse.pairStart(call("POST", "/pair", body = body))
    }

    /** Approved: the link. Otherwise throws the pairing codes (`authorization_pending`, …). */
    suspend fun pollPairing(deviceCode: String): Linked =
        Parse.linked(call("POST", "/pair/poll", body = JSONObject().put("deviceCode", deviceCode), retryRateLimit = false))

    suspend fun challenge(deviceId: String): Challenge =
        Parse.challenge(call("POST", "/session/challenge", body = JSONObject().put("deviceId", deviceId)))

    suspend fun session(deviceId: String, nonce: String, signature: String): SessionToken = Parse.session(
        call(
            "POST", "/session",
            body = JSONObject().put("deviceId", deviceId).put("nonce", nonce).put("signature", signature),
        ),
    )

    suspend fun me(token: String): Me = Parse.me(call("GET", "/me", token))

    suspend fun fleet(token: String): Fleet = Parse.fleet(call("GET", "/fleet", token))

    suspend fun review(token: String): List<ReviewItem> = Parse.reviewItems(call("GET", "/review", token))

    /** Decides an item as it was shown: its [ReviewItem.version] goes back with the decision. */
    suspend fun decide(token: String, item: ReviewItem, decision: Decision) {
        val version = requireNotNull(item.version) { "an item without a version is decided in the app" }
        val body = JSONObject()
            .put("agent", item.agent)
            .put("kind", item.kind)
            .put("id", item.id)
            .put("version", version)
            .put("decision", decision.wire)
        call("POST", "/review", token, body)
    }

    suspend fun conversations(token: String, agent: String): List<ConversationSummary> =
        Parse.conversations(call("GET", "/agents/${segment(agent)}/conversations", token))

    suspend fun conversation(token: String, agent: String, id: String, limit: Int = 30): Conversation =
        Parse.conversation(
            call("GET", "/agents/${segment(agent)}/conversations/${segment(id)}?limit=${limit.coerceIn(1, 100)}", token),
        )

    suspend fun chat(token: String, agent: String, conversation: String?, message: String): TurnStarted {
        require(message.isNotBlank() && message.length <= MAX_MESSAGE) { "a message is 1–$MAX_MESSAGE characters" }
        val body = JSONObject().put("message", message)
        if (conversation != null) body.put("conversation", conversation)
        return Parse.turnStarted(call("POST", "/agents/${segment(agent)}/chat", token, body))
    }

    suspend fun turn(token: String, turn: String): TurnStatus =
        Parse.turnStatus(call("GET", "/turns/${segment(turn)}", token))

    private suspend fun call(
        method: String,
        path: String,
        token: String? = null,
        body: JSONObject? = null,
        retryRateLimit: Boolean = true,
    ): JSONObject {
        val headers = HashMap<String, String>()
        if (token != null) headers["Authorization"] = "Bearer $token"
        val request = HttpRequest(method, "$baseUrl$PREFIX$path", headers, body?.toString())
        var retried = false
        while (true) {
            val response = try {
                withContext(io) { transport.execute(request) }
            } catch (e: IOException) {
                throw NetworkException(e)
            }
            if (response.status in 200..299) return Parse.obj(response.body.ifEmpty { "{}" })
            val error = ApiErrors.parse(response.status, response.body, response.header("Retry-After"))
            val wait = error.retryAfterSeconds
            if (error.code == ErrorCode.RATE_LIMITED && retryRateLimit && !retried && wait != null && wait <= MAX_AUTO_RETRY_SECONDS) {
                retried = true
                sleep(wait.coerceAtLeast(1) * 1000)
                continue
            }
            throw error
        }
    }

    private fun segment(s: String): String = URLEncoder.encode(s, "UTF-8").replace("+", "%20")

    companion object {
        const val PREFIX = "/device/v1"
        const val MAX_MESSAGE = 4000
        const val MAX_AUTO_RETRY_SECONDS = 30L
    }
}
