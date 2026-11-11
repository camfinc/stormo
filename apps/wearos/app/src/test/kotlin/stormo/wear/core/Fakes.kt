package stormo.wear.core

import kotlinx.coroutines.Dispatchers
import org.json.JSONObject
import java.io.IOException
import java.math.BigInteger
import java.security.AlgorithmParameters
import java.security.KeyFactory
import java.security.KeyPair
import java.security.KeyPairGenerator
import java.security.Signature
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec
import java.security.spec.ECParameterSpec
import java.security.spec.ECPoint
import java.security.spec.ECPublicKeySpec

/** A scripted gateway: each request takes the next answer, and every request is recorded. */
class FakeTransport(vararg answers: (HttpRequest) -> HttpResponse) : Transport {
    val requests = mutableListOf<HttpRequest>()
    private val queue = ArrayDeque(answers.toList())

    fun then(vararg more: (HttpRequest) -> HttpResponse) = apply { queue.addAll(more) }

    override fun execute(request: HttpRequest): HttpResponse {
        requests += request
        val next = queue.removeFirstOrNull() ?: throw AssertionError("unexpected ${request.method} ${request.url}")
        return next(request)
    }

    fun paths(): List<String> = requests.map { it.method + " " + it.url.removePrefix(BASE + GatewayClient.PREFIX) }

    fun body(index: Int): JSONObject = JSONObject(requests[index].body ?: error("request $index has no body"))
}

const val BASE = "https://gateway.example"

fun ok(body: String, status: Int = 200): (HttpRequest) -> HttpResponse = { HttpResponse(status, emptyMap(), body) }

fun err(status: Int, code: String, retryAfter: String? = null): (HttpRequest) -> HttpResponse = {
    HttpResponse(
        status,
        if (retryAfter != null) mapOf("retry-after" to retryAfter) else emptyMap(),
        """{"error":{"code":"$code","message":"from the gateway"}}""",
    )
}

fun offline(): (HttpRequest) -> HttpResponse = { throw IOException("connection reset") }

/** A virtual clock that only moves when the code under test sleeps. */
class FakeTime(var now: Long = 1_000_000L) {
    val sleeps = mutableListOf<Long>()
    val sleep: suspend (Long) -> Unit = { millis ->
        sleeps += millis
        now += millis
    }
    val clock: () -> Long = { now }
}

fun client(transport: Transport, time: FakeTime = FakeTime()) =
    GatewayClient(BASE, transport, io = Dispatchers.Unconfined, sleep = time.sleep)

/** A software P-256 key behind the same interface as the Keystore one. */
class SoftwareKey(val pair: KeyPair = generate()) : DeviceKey {
    override fun publicJwk(): EcPublicJwk = EcPublicJwk.fromPublicKey(pair.public as ECPublicKey)

    override fun sign(data: ByteArray): ByteArray = Signature.getInstance("SHA256withECDSA").run {
        initSign(pair.private)
        update(data)
        sign()
    }

    companion object {
        fun generate(): KeyPair = KeyPairGenerator.getInstance("EC").apply {
            initialize(ECGenParameterSpec("secp256r1"))
        }.generateKeyPair()
    }
}

class SoftwareKeys(var chain: List<ByteArray> = emptyList()) : DeviceKeys {
    var current: SoftwareKey? = null
    var created = 0

    override fun load(): DeviceKey? = current

    override fun create(): CreatedKey {
        val key = SoftwareKey()
        current = key
        created++
        return CreatedKey(key, chain, strongBox = false)
    }

    override fun delete() {
        current = null
    }
}

val P256: ECParameterSpec = AlgorithmParameters.getInstance("EC").run {
    init(ECGenParameterSpec("secp256r1"))
    getParameterSpec(ECParameterSpec::class.java)
}

/** The public key a gateway rebuilds from the JWK the watch sent. */
fun publicKeyOf(jwk: EcPublicJwk): ECPublicKey {
    val point = ECPoint(BigInteger(1, Base64Url.decode(jwk.x)), BigInteger(1, Base64Url.decode(jwk.y)))
    return KeyFactory.getInstance("EC").generatePublic(ECPublicKeySpec(point, P256)) as ECPublicKey
}

fun verifies(jwk: EcPublicJwk, message: String, signatureB64Url: String): Boolean =
    Signature.getInstance("SHA256withECDSA").run {
        initVerify(publicKeyOf(jwk))
        update(message.toByteArray(Charsets.UTF_8))
        verify(Base64Url.decode(signatureB64Url))
    }

/** The member names of a JSON object (android.jar's org.json has no keySet()). */
fun JSONObject.fieldNames(): Set<String> = keys().asSequence().toSet()
