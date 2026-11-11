package stormo.wear.core

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import kotlin.coroutines.CoroutineContext

/**
 * A linked device's session: challenge → sign → token. The token lives in this object only (never
 * on disk) and is renewed shortly before it expires. A call answered `unauthorized` (the token
 * expired, or the gateway restarted) opens a new session and is retried once.
 *
 * A locked watch does nothing: every call first checks [isLocked]; while locked the token is dropped
 * and [DeviceLockedException] is thrown without a request, so a copied-in-memory token is never used
 * by a watch nobody has unlocked.
 *
 * The link is lost when the gateway answers `device_unknown` (to the challenge, the session or the
 * call) or the key can never sign again ([KeyLostException]): [onLinkLost] runs (the app unlinks
 * locally) and the error reaches the caller. `bad_signature` reaches the caller as is.
 */
class DeviceSession(
    private val client: GatewayClient,
    val deviceId: String,
    private val key: DeviceKey,
    private val clock: () -> Long = System::currentTimeMillis,
    private val signing: CoroutineContext = Dispatchers.IO,
    private val isLocked: () -> Boolean = { false },
    private val onLinkLost: (StormoException) -> Unit = {},
) {
    private val mutex = Mutex()
    private var token: String? = null
    private var expiresAt = 0L

    suspend fun <T> call(block: suspend (token: String) -> T): T = try {
        callRetrying(block)
    } catch (e: GatewayException) {
        if (e.code == ErrorCode.DEVICE_UNKNOWN) onLinkLost(e)
        throw e
    } catch (e: KeyLostException) {
        onLinkLost(e)
        throw e
    }

    private suspend fun <T> callRetrying(block: suspend (token: String) -> T): T {
        val first = token()
        return try {
            block(first)
        } catch (e: GatewayException) {
            if (e.code != ErrorCode.UNAUTHORIZED) throw e
            forget(first)
            block(token())
        }
    }

    private suspend fun requireUnlocked() {
        if (isLocked()) {
            clear()
            throw DeviceLockedException()
        }
    }

    /** Drops the token; the next call opens a new session. */
    suspend fun clear() = mutex.withLock {
        token = null
        expiresAt = 0
    }

    private suspend fun forget(stale: String) = mutex.withLock {
        if (token == stale) {
            token = null
            expiresAt = 0
        }
    }

    /** The current token, or a new session's; never while the watch is locked. */
    private suspend fun token(): String {
        requireUnlocked()
        return mutex.withLock { tokenLocked() }
    }

    private suspend fun tokenLocked(): String {
        val current = token
        if (current != null && clock() < expiresAt) return current
        return open()
    }

    private suspend fun open(): String {
        var attempt = 0
        while (true) {
            val challenge = client.challenge(deviceId)
            val signature = withContext(signing) { SessionProof.sign(key, deviceId, challenge.nonce) }
            try {
                val session = client.session(deviceId, challenge.nonce, signature)
                token = session.token
                val life = session.expiresIn * 1000L
                expiresAt = clock() + maxOf(life / 2, life - RENEW_EARLY_MILLIS)
                return session.token
            } catch (e: GatewayException) {
                // The challenge expired or was replaced by another: ask for a new one, once.
                if (e.code != ErrorCode.EXPIRED_NONCE || attempt > 0) throw e
                attempt++
            }
        }
    }

    companion object {
        const val RENEW_EARLY_MILLIS = 60_000L
    }
}
