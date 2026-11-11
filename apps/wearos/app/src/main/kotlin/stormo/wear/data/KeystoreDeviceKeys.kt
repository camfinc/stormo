package stormo.wear.data

import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import stormo.wear.core.CreatedKey
import stormo.wear.core.DeviceKey
import stormo.wear.core.DeviceKeys
import stormo.wear.core.EcPublicJwk
import stormo.wear.core.KeyUnavailableException
import java.security.GeneralSecurityException
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.ProviderException
import java.security.SecureRandom
import java.security.Signature
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec

/**
 * The device key in the Android Keystore: EC P-256, sign only, SHA-256, usable only while the watch
 * is unlocked, never exportable. StrongBox is tried first, then the TEE; attestation is asked for
 * and dropped when the hardware refuses it.
 */
class KeystoreDeviceKeys(private val random: SecureRandom = SecureRandom()) : DeviceKeys {

    override fun load(): DeviceKey? {
        val entry = try {
            keyStore().getEntry(ALIAS, null) as? KeyStore.PrivateKeyEntry
        } catch (e: GeneralSecurityException) {
            throw KeyUnavailableException("the key store could not be read", e)
        } ?: return null
        val public = entry.certificate.publicKey as? ECPublicKey ?: return null
        return KeystoreDeviceKey(entry.privateKey, EcPublicJwk.fromPublicKey(public))
    }

    override fun create(): CreatedKey {
        delete()
        val challenge = ByteArray(32).also(random::nextBytes)
        var last: Exception? = null
        for (attempt in ATTEMPTS) {
            try {
                val generator = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, PROVIDER)
                generator.initialize(spec(attempt, challenge))
                generator.generateKeyPair()
                val chain = if (attempt.attest) {
                    keyStore().getCertificateChain(ALIAS)?.map { it.encoded }.orEmpty()
                } else {
                    emptyList()
                }
                val key = load() ?: throw KeyUnavailableException("the new key could not be read back")
                return CreatedKey(key, chain, attempt.strongBox)
            } catch (e: StrongBoxUnavailableException) {
                last = e
            } catch (e: ProviderException) {
                // Attestation refused, or the secure element failed: try the next, weaker option.
                last = e
            } catch (e: GeneralSecurityException) {
                last = e
            }
            deleteQuietly()
        }
        throw KeyUnavailableException("the watch could not make a key", last)
    }

    override fun delete() {
        try {
            val store = keyStore()
            if (store.containsAlias(ALIAS)) store.deleteEntry(ALIAS)
        } catch (e: GeneralSecurityException) {
            throw KeyUnavailableException("the key could not be deleted", e)
        }
    }

    private fun deleteQuietly() {
        try {
            delete()
        } catch (_: KeyUnavailableException) {
            // Nothing was left to delete.
        }
    }

    private fun spec(attempt: Attempt, challenge: ByteArray): KeyGenParameterSpec {
        val builder = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setUnlockedDeviceRequired(true)
            .setIsStrongBoxBacked(attempt.strongBox)
        if (attempt.attest) builder.setAttestationChallenge(challenge)
        return builder.build()
    }

    private fun keyStore(): KeyStore = KeyStore.getInstance(PROVIDER).apply { load(null) }

    private data class Attempt(val strongBox: Boolean, val attest: Boolean)

    companion object {
        const val ALIAS = "stormo-device"
        private const val PROVIDER = "AndroidKeyStore"
        private val ATTEMPTS = listOf(
            Attempt(strongBox = true, attest = true),
            Attempt(strongBox = false, attest = true),
            Attempt(strongBox = false, attest = false),
        )
    }
}

private class KeystoreDeviceKey(private val privateKey: PrivateKey, private val jwk: EcPublicJwk) : DeviceKey {
    override fun publicJwk(): EcPublicJwk = jwk

    override fun sign(data: ByteArray): ByteArray = try {
        Signature.getInstance("SHA256withECDSA").run {
            initSign(privateKey)
            update(data)
            sign()
        }
    } catch (e: GeneralSecurityException) {
        // Locked watch (UserNotAuthenticatedException), or the key was invalidated.
        throw KeyUnavailableException("the key could not sign", e)
    } catch (e: ProviderException) {
        throw KeyUnavailableException("the key could not sign", e)
    }
}
