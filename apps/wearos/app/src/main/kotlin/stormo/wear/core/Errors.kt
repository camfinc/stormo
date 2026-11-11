package stormo.wear.core

import org.json.JSONException
import org.json.JSONObject
import java.io.IOException

/** The error codes of the device API (docs/devices.md) a client branches on. */
enum class ErrorCode(val wire: String) {
    BAD_REQUEST("bad_request"),
    AUTHORIZATION_PENDING("authorization_pending"),
    SLOW_DOWN("slow_down"),
    EXPIRED_TOKEN("expired_token"),
    ACCESS_DENIED("access_denied"),
    EXPIRED_NONCE("expired_nonce"),
    APP_ONLY("app_only"),
    UNAUTHORIZED("unauthorized"),
    BAD_SIGNATURE("bad_signature"),
    DEVICE_UNKNOWN("device_unknown"),
    FORBIDDEN("forbidden"),
    NOT_FOUND("not_found"),
    CHANGED("changed"),
    BUSY("busy"),
    RATE_LIMITED("rate_limited"),
    CORE_UNREACHABLE("core_unreachable"),

    /** A code this client does not know, or an answer without one. */
    UNKNOWN("");

    companion object {
        fun fromWire(code: String?): ErrorCode =
            if (code.isNullOrEmpty()) UNKNOWN else entries.firstOrNull { it.wire == code } ?: UNKNOWN
    }
}

/** Everything the gateway layer throws. */
open class StormoException(message: String, cause: Throwable? = null) : Exception(message, cause)

/** The gateway answered with an error body (or a bare error status from the edge in front of it). */
class GatewayException(
    val status: Int,
    val code: ErrorCode,
    /** The code as sent, kept when [code] is [ErrorCode.UNKNOWN]. */
    val rawCode: String?,
    /** The gateway's message: for the user only after mapping, never logged with request data. */
    val serverMessage: String?,
    /** Seconds from `Retry-After`, when the answer carried one. */
    val retryAfterSeconds: Long?,
) : StormoException("gateway answered $status ${rawCode ?: code.wire}")

/** The gateway could not be reached (DNS, TLS, timeout, connection reset). */
class NetworkException(cause: IOException) : StormoException("network: ${cause.javaClass.simpleName}", cause)

/** The gateway answered something that is not the contract's JSON. */
class ProtocolException(message: String, cause: Throwable? = null) : StormoException(message, cause)

/** A call that needs a link (or a gateway address) before the watch has one. */
class NotLinkedException(message: String) : StormoException(message)

object ApiErrors {
    /** Parses `{"error": {"code", "message"}}`; an answer without one is classified by its status. */
    fun parse(status: Int, body: String, retryAfter: String?): GatewayException {
        var raw: String? = null
        var message: String? = null
        try {
            val error = JSONObject(body).optJSONObject("error")
            if (error != null) {
                raw = error.stringOrNull("code")
                message = error.stringOrNull("message")
            }
        } catch (_: JSONException) {
            // Not JSON (an edge's HTML error page, an empty body): classify by status below.
        }
        val known = ErrorCode.fromWire(raw)
        val code = if (known == ErrorCode.UNKNOWN && raw == null) byStatus(status) else known
        return GatewayException(status, code, raw, message, parseRetryAfter(retryAfter))
    }

    /** `Retry-After` in seconds; the HTTP-date form is not used by the gateway and reads as absent. */
    fun parseRetryAfter(value: String?): Long? = value?.trim()?.toLongOrNull()?.takeIf { it >= 0 }

    private fun byStatus(status: Int): ErrorCode = when (status) {
        401 -> ErrorCode.UNAUTHORIZED
        403 -> ErrorCode.FORBIDDEN
        404 -> ErrorCode.NOT_FOUND
        409 -> ErrorCode.BUSY
        429 -> ErrorCode.RATE_LIMITED
        502, 503, 504 -> ErrorCode.CORE_UNREACHABLE
        else -> ErrorCode.UNKNOWN
    }
}

/** What to tell the person wearing the watch. Never includes request data or tokens. */
object ErrorText {
    fun of(error: Throwable): String = when (error) {
        is NetworkException -> "Can't reach the gateway. Check the watch's connection."
        is KeyUnavailableException -> "The watch's key can't sign right now. Unlock the watch and try again."
        is ProtocolException -> "The gateway sent an unexpected answer."
        is NotLinkedException -> "Pair this watch first."
        is FingerprintMismatchException -> "The gateway reported a key this watch didn't make."
        is GatewayException -> when (error.code) {
            ErrorCode.CORE_UNREACHABLE -> "The fleet isn't answering. Try again later."
            ErrorCode.RATE_LIMITED ->
                error.retryAfterSeconds?.let { "Too many requests. Try again in $it s." }
                    ?: "Too many requests. Try again shortly."
            ErrorCode.FORBIDDEN -> "This watch isn't allowed to do that."
            ErrorCode.NOT_FOUND -> "That isn't there any more."
            ErrorCode.CHANGED -> "This changed; reload."
            ErrorCode.BUSY -> "A reply is already running on this watch. Wait for it to finish."
            ErrorCode.BAD_SIGNATURE ->
                "This watch's key doesn't match its link. Unlink it in Settings and pair again."
            ErrorCode.DEVICE_UNKNOWN -> "This watch is no longer linked. Pair it again."
            ErrorCode.UNAUTHORIZED, ErrorCode.EXPIRED_NONCE -> "The session ended. Try again."
            ErrorCode.APP_ONLY -> "This is decided in the app."
            ErrorCode.EXPIRED_TOKEN -> "The code expired."
            ErrorCode.ACCESS_DENIED -> "The pairing was refused."
            ErrorCode.BAD_REQUEST -> "The gateway refused the request."
            else -> "The gateway answered with an error (${error.status})."
        }
        else -> "Something went wrong."
    }
}

/** True for failures worth retrying later without changing anything. */
fun Throwable.isTransient(): Boolean = when (this) {
    is NetworkException -> true
    is GatewayException -> code == ErrorCode.CORE_UNREACHABLE
    else -> false
}
