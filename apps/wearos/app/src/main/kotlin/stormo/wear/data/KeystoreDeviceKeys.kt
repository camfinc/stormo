package stormo.wear.data

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import android.security.keystore.UserNotAuthenticatedException
import stormo.wear.core.CreatedKey
import stormo.wear.core.DeviceKey
import stormo.wear.core.DeviceKeys
import stormo.wear.core.EcPublicJwk
import stormo.wear.core.KeyFailureCause
import stormo.wear.core.KeyFailures
import stormo.wear.core.KeyHardware
import stormo.wear.core.KeySecurity
import stormo.wear.core.KeyUnavailableException
import stormo.wear.core.NoHardwareKeyStoreException
import java.security.GeneralSecurityException
import java.security.KeyFactory
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.ProviderException
import java.security.SecureRandom
import java.security.Signature
import java.security.UnrecoverableEntryException
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec

/**
 * The device key in the Android Keystore: EC P-256, sign only, SHA-256, usable only while the watch
 * is unlocked, never exportable, with an attestation chain when the hardware gives one.
 *
 * Generation tries StrongBox with attestation, then the TEE with attestation, then the TEE without
 * it. Whatever is generated is then checked with [KeyInfo]: a key that is not in secure hardware is
 * deleted and pairing stops ([NoHardwareKeyStoreException]); the link never rests on a software key.
 */
class KeystoreDeviceKeys(
    private val isLocked: () -> Boolean,
    private val isDeviceSecure: () -> Boolean,
    private val random: SecureRandom = SecureRandom(),
) : DeviceKeys {

    override fun load(): DeviceKey? {
        val entry = try {
            keyStore().getEntry(ALIAS, null) as? KeyStore.PrivateKeyEntry
        } catch (e: UnrecoverableEntryException) {
            // Includes UnrecoverableKeyException.
            throw failure(KeyFailureCause.UNRECOVERABLE, e)
        } catch (e: GeneralSecurityException) {
            throw KeyUnavailableException("the key store could not be read", e)
        } ?: return null
        val public = entry.certificate.publicKey as? ECPublicKey ?: return null
        return KeystoreDeviceKey(entry.privateKey, EcPublicJwk.fromPublicKey(public), ::failure)
    }

    override fun create(): CreatedKey {
        delete()
        val challenge = ByteArray(32).also(random::nextBytes)
        var last: Exception? = null
        for (attempt in ATTEMPTS) {
            try {
                val generator = KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, PROVIDER)
                generator.initialize(spec(attempt, challenge))
                val pair = generator.generateKeyPair()
                val level = security(pair.private)
                if (!KeyHardware.isHardwareBacked(level)) {
                    deleteQuietly()
                    throw NoHardwareKeyStoreException()
                }
                val chain = if (attempt.attest) {
                    keyStore().getCertificateChain(ALIAS)?.map { it.encoded }.orEmpty()
                } else {
                    emptyList()
                }
                val key = load() ?: throw KeyUnavailableException("the new key could not be read back")
                return CreatedKey(key, chain, strongBox = level == KeySecurity.STRONGBOX)
            } catch (e: StrongBoxUnavailableException) {
                // No StrongBox on this watch: the TEE is next.
                last = e
            } catch (e: ProviderException) {
                // The hardware refused this combination (most often the attestation): the next
                // attempt asks for less; the last one asks for no attestation at all.
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

    /** Where the key really lives, from the Keystore's own record of it. */
    private fun security(key: PrivateKey): KeySecurity {
        val info = KeyFactory.getInstance(key.algorithm, PROVIDER).getKeySpec(key, KeyInfo::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            return when (info.securityLevel) {
                KeyProperties.SECURITY_LEVEL_STRONGBOX -> KeySecurity.STRONGBOX
                KeyProperties.SECURITY_LEVEL_TRUSTED_ENVIRONMENT -> KeySecurity.TRUSTED_ENVIRONMENT
                KeyProperties.SECURITY_LEVEL_UNKNOWN_SECURE -> KeySecurity.UNKNOWN_SECURE
                KeyProperties.SECURITY_LEVEL_SOFTWARE -> KeySecurity.SOFTWARE
                else -> KeySecurity.UNKNOWN
            }
        }
        @Suppress("DEPRECATION")
        return if (info.isInsideSecureHardware) KeySecurity.TRUSTED_ENVIRONMENT else KeySecurity.SOFTWARE
    }

    /** Maps a refused key operation to what it means for the link. */
    private fun failure(error: Throwable): Exception = failure(cause(error), error)

    private fun failure(cause: KeyFailureCause, error: Throwable): Exception =
        KeyFailures.classify(cause, deviceLocked = isLocked(), deviceSecure = isDeviceSecure(), error = error)

    private fun cause(error: Throwable): KeyFailureCause {
        var e: Throwable? = error
        while (e != null) {
            when (e) {
                is KeyPermanentlyInvalidatedException -> return KeyFailureCause.INVALIDATED
                is UserNotAuthenticatedException -> return KeyFailureCause.USER_NOT_AUTHENTICATED
                is UnrecoverableEntryException -> return KeyFailureCause.UNRECOVERABLE
            }
            e = e.cause
        }
        return KeyFailureCause.OTHER
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

private class KeystoreDeviceKey(
    private val privateKey: PrivateKey,
    private val jwk: EcPublicJwk,
    private val failure: (Throwable) -> Exception,
) : DeviceKey {
    override fun publicJwk(): EcPublicJwk = jwk

    override fun sign(data: ByteArray): ByteArray = try {
        Signature.getInstance("SHA256withECDSA").run {
            initSign(privateKey)
            update(data)
            sign()
        }
    } catch (e: GeneralSecurityException) {
        throw failure(e)
    } catch (e: ProviderException) {
        throw failure(e)
    }
}
