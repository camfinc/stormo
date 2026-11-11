package stormo.wear.core

/**
 * The device's signing key. On a watch it lives in the Android Keystore and never leaves it
 * (data/KeystoreDeviceKeys.kt); the JVM tests use a software key.
 */
interface DeviceKey {
    /** The public half as a JWK. */
    fun publicJwk(): EcPublicJwk

    /**
     * ECDSA P-256 with SHA-256 over [data], ASN.1 DER encoded. Throws [DeviceLockedException] while
     * the watch is locked, [KeyLostException] when the key can never sign again, and
     * [KeyUnavailableException] for any other refusal.
     */
    fun sign(data: ByteArray): ByteArray
}

/** Where the one device key is made, found and deleted. */
interface DeviceKeys {
    /** The current key, or null when there is none. */
    fun load(): DeviceKey?

    /**
     * Makes a new key, replacing any old one. Throws [NoHardwareKeyStoreException] when the only key
     * the watch can make is a software one.
     */
    fun create(): CreatedKey

    /** Deletes the key; nothing can sign as this device afterwards. */
    fun delete()
}

/**
 * A new key and, when the hardware gave one, its attestation chain (DER certificates, leaf first).
 * [strongBox] says whether the key lives in a StrongBox secure element rather than the TEE.
 */
class CreatedKey(val key: DeviceKey, val attestationChain: List<ByteArray>, val strongBox: Boolean)

/** The key refused to sign for a reason that may pass (retry later). */
class KeyUnavailableException(message: String, cause: Throwable? = null) : StormoException(message, cause)

/** The watch is locked: nothing is signed or sent, and the session token is dropped. */
class DeviceLockedException(cause: Throwable? = null) : StormoException("the watch is locked", cause)

/**
 * The key can never sign again (permanently invalidated, unrecoverable, or missing): the link is
 * forgotten and the watch pairs again. [screenLockMissing]: the watch has no secure screen lock,
 * which is the usual reason a key was invalidated.
 */
class KeyLostException(val screenLockMissing: Boolean, cause: Throwable? = null) :
    StormoException("the device key can no longer be used", cause)

/** The watch could only make a software key, which a link must not rest on. */
class NoHardwareKeyStoreException : StormoException("the watch has no hardware-backed key store")

/** Why the Keystore refused the key, as the Android layer classifies its exception. */
enum class KeyFailureCause {
    /** `KeyPermanentlyInvalidatedException`. */
    INVALIDATED,

    /** `UnrecoverableEntryException` (and its `UnrecoverableKeyException`). */
    UNRECOVERABLE,

    /** The alias holds no key. */
    MISSING,

    /** `UserNotAuthenticatedException`: the key needs the watch unlocked. */
    USER_NOT_AUTHENTICATED,
    OTHER,
}

object KeyFailures {
    /** What a refused key operation means for the link. */
    fun classify(cause: KeyFailureCause, deviceLocked: Boolean, deviceSecure: Boolean, error: Throwable? = null): StormoException =
        when (cause) {
            KeyFailureCause.INVALIDATED, KeyFailureCause.UNRECOVERABLE, KeyFailureCause.MISSING ->
                KeyLostException(screenLockMissing = !deviceSecure, cause = error)
            KeyFailureCause.USER_NOT_AUTHENTICATED -> DeviceLockedException(error)
            // An unnamed refusal while the watch is locked is the lock (unlocked-device-required keys).
            KeyFailureCause.OTHER ->
                if (deviceLocked) DeviceLockedException(error) else KeyUnavailableException("the key could not sign", error)
        }
}

/** Where a Keystore key lives (`KeyInfo.securityLevel` on API 31+, `isInsideSecureHardware` before). */
enum class KeySecurity { STRONGBOX, TRUSTED_ENVIRONMENT, UNKNOWN_SECURE, SOFTWARE, UNKNOWN }

object KeyHardware {
    /** Only a key in secure hardware (StrongBox or the TEE) may back a link. */
    fun isHardwareBacked(level: KeySecurity): Boolean =
        level == KeySecurity.STRONGBOX || level == KeySecurity.TRUSTED_ENVIRONMENT || level == KeySecurity.UNKNOWN_SECURE
}
