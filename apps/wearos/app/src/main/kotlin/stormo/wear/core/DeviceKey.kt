package stormo.wear.core

/**
 * The device's signing key. On a watch it lives in the Android Keystore and never leaves it
 * (data/KeystoreDeviceKeys.kt); the JVM tests use a software key.
 */
interface DeviceKey {
    /** The public half as a JWK. */
    fun publicJwk(): EcPublicJwk

    /**
     * ECDSA P-256 with SHA-256 over [data], ASN.1 DER encoded.
     * Throws [KeyUnavailableException] when the key cannot sign (the watch is locked, the key is gone).
     */
    fun sign(data: ByteArray): ByteArray
}

/** Where the one device key is made, found and deleted. */
interface DeviceKeys {
    /** The current key, or null when there is none. */
    fun load(): DeviceKey?

    /** Makes a new key, replacing any old one. */
    fun create(): CreatedKey

    /** Deletes the key; nothing can sign as this device afterwards. */
    fun delete()
}

/**
 * A new key and, when the hardware gave one, its attestation chain (DER certificates, leaf first).
 * [strongBox] says whether the key lives in a StrongBox secure element rather than the TEE.
 */
class CreatedKey(val key: DeviceKey, val attestationChain: List<ByteArray>, val strongBox: Boolean)

class KeyUnavailableException(message: String, cause: Throwable? = null) : StormoException(message, cause)
