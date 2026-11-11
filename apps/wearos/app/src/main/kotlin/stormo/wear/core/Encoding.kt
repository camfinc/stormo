package stormo.wear.core

import java.math.BigInteger
import java.security.MessageDigest
import java.security.interfaces.ECPublicKey
import java.util.Base64

/** Base64url without padding (RFC 4648 §5), as the device API uses for keys, nonces and signatures. */
object Base64Url {
    private val encoder = Base64.getUrlEncoder().withoutPadding()
    private val decoder = Base64.getUrlDecoder()

    fun encode(bytes: ByteArray): String = encoder.encodeToString(bytes)

    /** Accepts input with or without padding; throws [IllegalArgumentException] on anything else. */
    fun decode(text: String): ByteArray = decoder.decode(text.trimEnd('='))
}

/** An EC P-256 public key as a JWK (RFC 7517/7518): `x` and `y` are base64url of 32 bytes each. */
data class EcPublicJwk(val x: String, val y: String) {

    /**
     * The RFC 7638 thumbprint input: the required members in lexicographic order, no whitespace.
     * The fingerprint the owner compares is computed over exactly these bytes.
     */
    fun thumbprintInput(): String = "{\"crv\":\"$CRV\",\"kty\":\"$KTY\",\"x\":\"$x\",\"y\":\"$y\"}"

    companion object {
        const val KTY = "EC"
        const val CRV = "P-256"
        const val COORDINATE_BYTES = 32

        fun fromPublicKey(key: ECPublicKey): EcPublicJwk =
            fromCoordinates(key.w.affineX, key.w.affineY)

        fun fromCoordinates(x: BigInteger, y: BigInteger): EcPublicJwk =
            EcPublicJwk(Base64Url.encode(coordinate(x)), Base64Url.encode(coordinate(y)))

        /**
         * A curve coordinate as exactly 32 big-endian bytes. [BigInteger.toByteArray] adds a leading
         * sign byte when the top bit is set (33 bytes) and drops leading zeros (fewer than 32).
         */
        fun coordinate(value: BigInteger): ByteArray {
            require(value.signum() >= 0) { "a curve coordinate is never negative" }
            val raw = value.toByteArray()
            if (raw.size == COORDINATE_BYTES) return raw
            if (raw.size > COORDINATE_BYTES) {
                val extra = raw.size - COORDINATE_BYTES
                require((0 until extra).all { raw[it] == 0.toByte() }) { "coordinate is longer than 32 bytes" }
                return raw.copyOfRange(extra, raw.size)
            }
            return ByteArray(COORDINATE_BYTES - raw.size) + raw
        }
    }
}

/**
 * The key fingerprint both sides show before a link: the first 12 hex digits (upper case) of SHA-256
 * over the JWK thumbprint input, grouped `XXXX-XXXX-XXXX`.
 */
object Fingerprint {
    private const val HEX = "0123456789ABCDEF"

    fun of(jwk: EcPublicJwk): String {
        val digest = MessageDigest.getInstance("SHA-256")
            .digest(jwk.thumbprintInput().toByteArray(Charsets.UTF_8))
        val hex = StringBuilder()
        for (i in 0 until 6) {
            val b = digest[i].toInt() and 0xFF
            hex.append(HEX[b ushr 4]).append(HEX[b and 0x0F])
        }
        return hex.chunked(4).joinToString("-")
    }

    /** Equal ignoring case and grouping. */
    fun matches(a: String, b: String): Boolean = normalize(a) == normalize(b) && normalize(a).isNotEmpty()

    private fun normalize(s: String) = s.filter { it.isLetterOrDigit() }.uppercase()
}

/** The proof a linked device signs to open a session. */
object SessionProof {
    const val PREFIX = "stormo-device-session"

    /** Three lines joined by `\n`, no trailing newline. */
    fun message(deviceId: String, nonce: String): String = "$PREFIX\n$deviceId\n$nonce"

    /** Signs the proof with the device key; the DER signature as base64url without padding. */
    fun sign(key: DeviceKey, deviceId: String, nonce: String): String =
        Base64Url.encode(key.sign(message(deviceId, nonce).toByteArray(Charsets.UTF_8)))
}

/** The pairing code the owner types: 8 letters, shown `XXXX-XXXX`. */
object UserCode {
    fun display(code: String): String {
        val letters = code.filter { it.isLetterOrDigit() }.uppercase()
        return if (letters.length == 8) letters.substring(0, 4) + "-" + letters.substring(4) else letters
    }

    /** For a screen reader: one letter at a time. */
    fun spoken(code: String): String = display(code).filter { it.isLetterOrDigit() }.toList().joinToString(" ")
}

/**
 * The attestation chain as the pairing request carries it: standard base64 of each DER certificate,
 * leaf first. The gateway takes up to 8 certificates of up to 8 KB each; a chain that does not fit
 * is left out (attestation is optional) rather than refused.
 */
object Attestation {
    const val MAX_CERTIFICATES = 8
    const val MAX_CERTIFICATE_BYTES = 8 * 1024

    fun encode(chain: List<ByteArray>): List<String> {
        if (chain.isEmpty() || chain.size > MAX_CERTIFICATES) return emptyList()
        if (chain.any { it.isEmpty() || it.size > MAX_CERTIFICATE_BYTES }) return emptyList()
        val encoder = Base64.getEncoder()
        return chain.map { encoder.encodeToString(it) }
    }
}
