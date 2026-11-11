package stormo.wear.core

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlin.coroutines.CoroutineContext

/** How a pairing ended. */
sealed interface PairingOutcome {
    data class Approved(val linked: Linked) : PairingOutcome
    data object Expired : PairingOutcome
    data object Denied : PairingOutcome
    data class Failed(val error: StormoException) : PairingOutcome
}

/** A pairing the gateway accepted: the code to show and the key's fingerprint, checked locally. */
data class PairingTicket(val start: PairStart, val fingerprint: String)

/** The gateway reported a fingerprint for a key that is not this watch's key. */
class FingerprintMismatchException(val local: String, val remote: String) :
    StormoException("the gateway's fingerprint does not match this watch's key")

/**
 * The pairing poll as a pure rule (RFC 8628 §3.5 and docs/devices.md): what to do after each answer.
 */
object PairingPolicy {
    const val SLOW_DOWN_SECONDS = 5

    sealed interface Step {
        /** Poll again after [interval] seconds (plus [extraWaitSeconds] once, for a rate limit). */
        data class Continue(val interval: Int, val extraWaitSeconds: Long = 0) : Step
        data class Finish(val outcome: PairingOutcome) : Step
    }

    fun afterApproval(linked: Linked): Step = Step.Finish(PairingOutcome.Approved(linked))

    fun afterError(interval: Int, error: StormoException): Step = when {
        error is GatewayException -> when (error.code) {
            ErrorCode.AUTHORIZATION_PENDING -> Step.Continue(interval)
            ErrorCode.SLOW_DOWN -> Step.Continue(interval + SLOW_DOWN_SECONDS)
            ErrorCode.EXPIRED_TOKEN -> Step.Finish(PairingOutcome.Expired)
            ErrorCode.ACCESS_DENIED -> Step.Finish(PairingOutcome.Denied)
            ErrorCode.RATE_LIMITED -> Step.Continue(interval, extraWaitSeconds = error.retryAfterSeconds ?: interval.toLong())
            ErrorCode.CORE_UNREACHABLE -> Step.Continue(interval)
            else -> Step.Finish(PairingOutcome.Failed(error))
        }
        // The watch lost its connection for a moment: keep polling until the code expires.
        error is NetworkException -> Step.Continue(interval)
        else -> Step.Finish(PairingOutcome.Failed(error))
    }
}

/**
 * Pairing: make a key, ask the gateway for a code, check the fingerprint it reports, then poll until
 * the owner types the code (or refuses it, or it expires).
 */
class Pairing(
    private val client: GatewayClient,
    private val keys: DeviceKeys,
    private val info: DeviceInfo,
    private val sleep: suspend (Long) -> Unit,
    private val clock: () -> Long = System::currentTimeMillis,
    private val io: CoroutineContext = Dispatchers.IO,
) {
    /**
     * Starts a pairing with a new key. Throws [FingerprintMismatchException] when the gateway's
     * fingerprint is not this key's: the code is then never shown, so nobody can link a key the
     * watch did not make.
     */
    suspend fun begin(): PairingTicket {
        val created = withContext(io) { keys.create() }
        val jwk = created.key.publicJwk()
        val local = Fingerprint.of(jwk)
        val attestation = Attestation.encode(created.attestationChain)
        val start = try {
            client.pair(info, jwk, attestation)
        } catch (e: GatewayException) {
            // A gateway that refuses the chain still gets the pairing: attestation is optional.
            if (e.code != ErrorCode.BAD_REQUEST || attestation.isEmpty()) throw e
            client.pair(info, jwk, emptyList())
        }
        if (!Fingerprint.matches(local, start.fingerprint)) throw FingerprintMismatchException(local, start.fingerprint)
        return PairingTicket(start, local)
    }

    /** Polls every `interval` seconds until a final answer or the code's expiry. */
    suspend fun await(start: PairStart): PairingOutcome {
        var interval = start.interval.coerceAtLeast(1)
        val deadline = clock() + start.expiresIn * 1000L
        var extraWait = 0L
        while (true) {
            sleep((interval + extraWait) * 1000L)
            val step = try {
                PairingPolicy.afterApproval(client.pollPairing(start.deviceCode))
            } catch (e: StormoException) {
                PairingPolicy.afterError(interval, e)
            }
            when (step) {
                is PairingPolicy.Step.Finish -> return step.outcome
                is PairingPolicy.Step.Continue -> {
                    // A final answer is always taken; only waiting stops at the code's expiry.
                    if (clock() >= deadline) return PairingOutcome.Expired
                    interval = step.interval
                    extraWait = step.extraWaitSeconds
                }
            }
        }
    }
}
