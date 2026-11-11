package stormo.wear.core

import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

class ErrorParsingTest {
    @Test
    fun everyContractCodeMapsToItsType() {
        val codes = mapOf(
            "bad_request" to ErrorCode.BAD_REQUEST,
            "authorization_pending" to ErrorCode.AUTHORIZATION_PENDING,
            "slow_down" to ErrorCode.SLOW_DOWN,
            "expired_token" to ErrorCode.EXPIRED_TOKEN,
            "access_denied" to ErrorCode.ACCESS_DENIED,
            "expired_nonce" to ErrorCode.EXPIRED_NONCE,
            "app_only" to ErrorCode.APP_ONLY,
            "unauthorized" to ErrorCode.UNAUTHORIZED,
            "bad_signature" to ErrorCode.BAD_SIGNATURE,
            "device_unknown" to ErrorCode.DEVICE_UNKNOWN,
            "forbidden" to ErrorCode.FORBIDDEN,
            "not_found" to ErrorCode.NOT_FOUND,
            "changed" to ErrorCode.CHANGED,
            "busy" to ErrorCode.BUSY,
            "rate_limited" to ErrorCode.RATE_LIMITED,
            "core_unreachable" to ErrorCode.CORE_UNREACHABLE,
        )
        assertEquals(ErrorCode.entries.size - 1, codes.size) // every code but UNKNOWN
        for ((wire, code) in codes) {
            val e = ApiErrors.parse(400, """{"error":{"code":"$wire","message":"text"}}""", null)
            assertEquals(wire, code, e.code)
            assertEquals("text", e.serverMessage)
            assertEquals(wire, e.rawCode)
        }
    }

    @Test
    fun anUnknownCodeIsKeptRaw() {
        val e = ApiErrors.parse(400, """{"error":{"code":"brand_new","message":"m"}}""", null)
        assertEquals(ErrorCode.UNKNOWN, e.code)
        assertEquals("brand_new", e.rawCode)
    }

    @Test
    fun anAnswerWithoutABodyIsClassifiedByStatus() {
        assertEquals(ErrorCode.CORE_UNREACHABLE, ApiErrors.parse(502, "<html>Bad gateway</html>", null).code)
        assertEquals(ErrorCode.CORE_UNREACHABLE, ApiErrors.parse(503, "", null).code)
        assertEquals(ErrorCode.RATE_LIMITED, ApiErrors.parse(429, "", "12").code)
        assertEquals(ErrorCode.UNAUTHORIZED, ApiErrors.parse(401, "", null).code)
        assertEquals(ErrorCode.NOT_FOUND, ApiErrors.parse(404, "{}", null).code)
        assertEquals(ErrorCode.UNKNOWN, ApiErrors.parse(500, "{}", null).code)
    }

    @Test
    fun retryAfterIsReadInSeconds() {
        assertEquals(12L, ApiErrors.parse(429, """{"error":{"code":"rate_limited","message":"m"}}""", "12").retryAfterSeconds)
        assertEquals(0L, ApiErrors.parseRetryAfter(" 0 "))
        assertNull(ApiErrors.parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT"))
        assertNull(ApiErrors.parseRetryAfter("-3"))
        assertNull(ApiErrors.parseRetryAfter(null))
    }

    @Test
    fun userTextNeverEchoesTheServerMessage() {
        val e = GatewayException(429, ErrorCode.RATE_LIMITED, "rate_limited", "secret detail", 7)
        assertEquals("Too many requests. Try again in 7 s.", ErrorText.of(e))
        assertFalse(ErrorText.of(GatewayException(500, ErrorCode.UNKNOWN, "x", "secret detail", null)).contains("secret"))
    }
}

class GatewayClientTest {
    private val info = DeviceInfo("Watch", "Pixel Watch 3", "Wear OS 6", "0.1.0")

    @Test
    fun pairSendsTheContractsBody() = runBlocking {
        val transport = FakeTransport(
            ok("""{"deviceCode":"dc","userCode":"WDJB-MJHT","fingerprint":"3F2A-91C0-7B4E","expiresIn":600,"interval":5}"""),
            ok("""{"deviceCode":"dc2","userCode":"BCDF-GHJK","fingerprint":"3F2A-91C0-7B4E","expiresIn":600,"interval":5}"""),
        )
        val c = client(transport)
        val jwk = EcPublicJwk("xx", "yy")
        val start = c.pair(info, jwk, listOf("AQID"))
        assertEquals(PairStart("dc", "WDJB-MJHT", "3F2A-91C0-7B4E", 600, 5), start)

        assertEquals("POST /pair", transport.paths()[0])
        assertNull(transport.requests[0].headers["Authorization"])
        val body = transport.body(0)
        assertEquals(setOf("name", "model", "os", "appVersion", "publicKey", "attestation"), body.fieldNames())
        assertEquals("Watch", body.getString("name"))
        assertEquals("Pixel Watch 3", body.getString("model"))
        assertEquals("Wear OS 6", body.getString("os"))
        assertEquals("0.1.0", body.getString("appVersion"))
        val key = body.getJSONObject("publicKey")
        assertEquals(setOf("kty", "crv", "x", "y"), key.fieldNames())
        assertEquals("EC", key.getString("kty"))
        assertEquals("P-256", key.getString("crv"))
        assertEquals("xx", key.getString("x"))
        assertEquals("AQID", body.getJSONArray("attestation").getString(0))

        c.pair(info, jwk, emptyList())
        assertFalse("no attestation, no field", transport.body(1).has("attestation"))
    }

    @Test
    fun authenticatedRoutesCarryTheBearerTokenAndEncodePaths() = runBlocking {
        val transport = FakeTransport(
            ok("""{"conversations":[{"id":"c 1","title":"Refunds","preview":"…","lastActive":"2026-11-10T21:30:00Z","messages":4}]}"""),
            ok("""{"id":"c 1","compacted":true,"messages":[{"id":"m1","role":"user","content":"hi","at":"2026-11-10T21:30:00Z"},{"id":"m2","role":"assistant","content":"hello"}]}"""),
            ok("""{"turn":"trn_0123456789abcdef","conversation":"c 1"}""", status = 202),
        )
        val c = client(transport)
        val list = c.conversations("std_tok", "support")
        assertEquals("Refunds", list.single().title)
        assertEquals(4, list.single().messages)
        val conv = c.conversation("std_tok", "support", "c 1", limit = 500)
        assertTrue(conv.compacted)
        assertTrue(conv.messages[0].fromUser)
        assertNull(conv.messages[1].at)
        val started = c.chat("std_tok", "support", "c 1", "Any refunds today?")
        assertEquals(TurnStarted("trn_0123456789abcdef", "c 1"), started)

        assertEquals(
            listOf(
                "GET /agents/support/conversations",
                "GET /agents/support/conversations/c%201?limit=100",
                "POST /agents/support/chat",
            ),
            transport.paths(),
        )
        transport.requests.forEach { assertEquals("Bearer std_tok", it.headers["Authorization"]) }
        val body = transport.body(2)
        assertEquals("c 1", body.getString("conversation"))
        assertEquals("Any refunds today?", body.getString("message"))
    }

    @Test
    fun aNewConversationOmitsTheField() = runBlocking {
        val transport = FakeTransport(ok("""{"turn":"trn_1","conversation":"new"}""", status = 202))
        client(transport).chat("t", "support", null, "Hello")
        assertFalse(transport.body(0).has("conversation"))
    }

    @Test
    fun readsMeFleetAndTurns() = runBlocking {
        val transport = FakeTransport(
            ok("""{"device":{"id":"dev_1","name":"Watch","model":"Pixel Watch 3","grants":{"status":true,"review":false,"chat":["support"]},"linked":1762790000000},"tokenExpires":1762793600000,"instance":"Acme"}"""),
            ok("""{"agents":[{"id":"support","name":"Ada","unit":"ops","state":"running","busy":true,"live":null,"waiting":0,"unread":2,"pendingReview":3}],"polledAt":"2026-11-10T21:30:00Z"}"""),
            ok("""{"turn":"trn_1","state":"done","conversation":"c1","reply":"Done."}"""),
            ok("""{"turn":"trn_2","state":"failed","conversation":"c1","error":"the agent stopped"}"""),
        )
        val c = client(transport)
        val me = c.me("t")
        assertEquals("Acme", me.instance)
        assertTrue(me.grants.status)
        assertFalse(me.grants.review)
        assertTrue(me.grants.canChat("support"))
        assertFalse(me.grants.canChat("billing"))
        val agent = c.fleet("t").agents.single()
        assertEquals("Ada", agent.name)
        assertNull("JSON null is no tool", agent.live)
        assertEquals(3, agent.pendingReview)
        assertEquals(TurnState.DONE, c.turn("t", "trn_1").state)
        val failed = c.turn("t", "trn_2")
        assertEquals(TurnState.FAILED, failed.state)
        assertEquals("the agent stopped", failed.error)
    }

    @Test
    fun theWildcardGrantCoversEveryAgent() {
        val grants = Grants(status = false, review = false, chat = listOf("*"))
        assertTrue(grants.canChat("anything"))
        assertFalse(Grants.NONE.canChat("support"))
    }

    @Test
    fun aShortRateLimitIsWaitedOutOnce() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(err(429, "rate_limited", retryAfter = "3"), ok("""{"status":"ok"}"""))
        assertTrue(client(transport, time).health())
        assertEquals(listOf(3_000L), time.sleeps)
        assertEquals(2, transport.requests.size)
    }

    @Test
    fun aLongOrRepeatedRateLimitIsThrownWithItsWait() = runBlocking {
        val time = FakeTime()
        val long = FakeTransport(err(429, "rate_limited", retryAfter = "120"))
        val e = expect<GatewayException> { client(long, time).health() }
        assertEquals(ErrorCode.RATE_LIMITED, e.code)
        assertEquals(120L, e.retryAfterSeconds)
        assertTrue(time.sleeps.isEmpty())

        val repeated = FakeTransport(err(429, "rate_limited", retryAfter = "1"), err(429, "rate_limited", retryAfter = "1"))
        assertEquals(ErrorCode.RATE_LIMITED, expect<GatewayException> { client(repeated, time).health() }.code)
        assertEquals(2, repeated.requests.size)
    }

    @Test
    fun failuresAreTyped() = runBlocking {
        assertEquals(ErrorCode.FORBIDDEN, expect<GatewayException> { client(FakeTransport(err(403, "forbidden"))).fleet("t") }.code)
        expect<NetworkException> { client(FakeTransport(offline())).health() }
        expect<ProtocolException> { client(FakeTransport(ok("not json"))).fleet("t") }
        expect<ProtocolException> { client(FakeTransport(ok("""{"turn":"t"}"""))).turn("t", "t") }
        Unit
    }

    @Test
    fun messagesOutsideTheLimitsAreNotSent() {
        val transport = FakeTransport()
        assertThrows(IllegalArgumentException::class.java) {
            runBlocking { client(transport).chat("t", "support", null, "x".repeat(4001)) }
        }
        assertTrue(transport.requests.isEmpty())
        assertEquals(null, Chat.problem("x".repeat(4000)))
        assertTrue(Chat.problem("   ") != null)
    }
}

suspend inline fun <reified E : Throwable> expect(crossinline block: suspend () -> Unit): E {
    try {
        block()
    } catch (e: Throwable) {
        if (e is E) return e
        throw AssertionError("expected ${E::class.simpleName}, got $e", e)
    }
    fail("expected ${E::class.simpleName}")
    throw IllegalStateException()
}
