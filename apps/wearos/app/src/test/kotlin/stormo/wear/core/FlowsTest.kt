package stormo.wear.core

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

private const val LINKED = """{"deviceId":"dev_0123456789abcdef","name":"Ada's watch"}"""

class PairingPollTest {
    private val start = PairStart("dc", "WDJB-MJHT", "3F2A-91C0-7B4E", expiresIn = 600, interval = 5)

    private fun pairing(transport: FakeTransport, time: FakeTime) =
        Pairing(client(transport, time), SoftwareKeys(), DeviceInfo.of("Pixel Watch 3", 36, "16", "0.1.0"), time.sleep, time.clock, Dispatchers.Unconfined)

    @Test
    fun pendingAndSlowDownUntilApproved() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(
            err(400, "authorization_pending"),
            err(400, "slow_down"),
            err(400, "authorization_pending"),
            ok(LINKED),
        )
        val outcome = pairing(transport, time).await(start)
        assertEquals(PairingOutcome.Approved(Linked("dev_0123456789abcdef", "Ada's watch")), outcome)
        // Every interval, plus 5 s from slow_down on.
        assertEquals(listOf(5_000L, 5_000L, 10_000L, 10_000L), time.sleeps)
        assertEquals(List(4) { "POST /pair/poll" }, transport.paths())
        assertEquals("dc", transport.body(0).getString("deviceCode"))
    }

    @Test
    fun slowDownAccumulates() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(err(400, "slow_down"), err(400, "slow_down"), ok(LINKED))
        pairing(transport, time).await(start)
        assertEquals(listOf(5_000L, 10_000L, 15_000L), time.sleeps)
    }

    @Test
    fun expiredAndDeniedEndThePairing() = runBlocking {
        assertEquals(
            PairingOutcome.Expired,
            pairing(FakeTransport(err(400, "authorization_pending"), err(400, "expired_token")), FakeTime()).await(start),
        )
        assertEquals(PairingOutcome.Denied, pairing(FakeTransport(err(400, "access_denied")), FakeTime()).await(start))
    }

    @Test
    fun anotherErrorFailsThePairing() = runBlocking {
        val outcome = pairing(FakeTransport(err(400, "bad_request")), FakeTime()).await(start)
        assertTrue(outcome is PairingOutcome.Failed)
        assertEquals(ErrorCode.BAD_REQUEST, ((outcome as PairingOutcome.Failed).error as GatewayException).code)
    }

    @Test
    fun outagesAndRateLimitsAreRiddenOut() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(
            offline(),
            err(503, "core_unreachable"),
            err(429, "rate_limited", retryAfter = "7"),
            ok(LINKED),
        )
        val outcome = pairing(transport, time).await(start)
        assertTrue(outcome is PairingOutcome.Approved)
        // The rate limit's Retry-After is waited on top of the interval, once.
        assertEquals(listOf(5_000L, 5_000L, 5_000L, 12_000L), time.sleeps)
    }

    @Test
    fun waitingStopsAtTheCodesExpiry() = runBlocking {
        val time = FakeTime()
        val short = start.copy(expiresIn = 12)
        val transport = FakeTransport(err(400, "authorization_pending"), offline(), err(400, "authorization_pending"))
        assertEquals(PairingOutcome.Expired, pairing(transport, time).await(short))
        assertEquals(3, transport.requests.size)
    }

    @Test
    fun theRuleItself() {
        val pending = GatewayException(400, ErrorCode.AUTHORIZATION_PENDING, null, null, null)
        assertEquals(PairingPolicy.Step.Continue(5), PairingPolicy.afterError(5, pending))
        val slow = GatewayException(400, ErrorCode.SLOW_DOWN, null, null, null)
        assertEquals(PairingPolicy.Step.Continue(10), PairingPolicy.afterError(5, slow))
        val unknown = GatewayException(401, ErrorCode.DEVICE_UNKNOWN, null, null, null)
        assertTrue(PairingPolicy.afterError(5, unknown) is PairingPolicy.Step.Finish)
    }
}

class PairingBeginTest {
    private val info = DeviceInfo.of("Pixel Watch 3", 36, "16", "0.1.0")

    private fun answer(fingerprint: String) =
        ok("""{"deviceCode":"dc","userCode":"WDJBMJHT","fingerprint":"$fingerprint","expiresIn":600,"interval":5}""")

    @Test
    fun sendsANewKeyAndChecksTheFingerprint() = runBlocking {
        val keys = SoftwareKeys(chain = listOf(byteArrayOf(1, 2, 3)))
        val transport = FakeTransport()
        val time = FakeTime()
        val pairing = Pairing(client(transport, time), keys, info, time.sleep, time.clock, Dispatchers.Unconfined)
        transport.then({ request ->
            // The gateway computes the fingerprint from the key it received.
            val key = org.json.JSONObject(request.body!!).getJSONObject("publicKey")
            answer(Fingerprint.of(EcPublicJwk(key.getString("x"), key.getString("y"))))(request)
        })
        val ticket = pairing.begin()
        val jwk = keys.current!!.publicJwk()
        assertEquals(1, keys.created)
        assertEquals(Fingerprint.of(jwk), ticket.fingerprint)
        assertEquals(jwk.x, transport.body(0).getJSONObject("publicKey").getString("x"))
        assertEquals("AQID", transport.body(0).getJSONArray("attestation").getString(0))
    }

    @Test
    fun aDifferentFingerprintStopsThePairingBeforeTheCodeIsShown() = runBlocking {
        val time = FakeTime()
        val pairing = Pairing(client(FakeTransport(answer("0000-0000-0000")), time), SoftwareKeys(), info, time.sleep, time.clock, Dispatchers.Unconfined)
        val e = expect<FingerprintMismatchException> { pairing.begin() }
        assertEquals("0000-0000-0000", e.remote)
        assertFalse(e.local == e.remote)
    }

    @Test
    fun aRefusedAttestationIsLeftOut() = runBlocking {
        val keys = SoftwareKeys(chain = listOf(byteArrayOf(9)))
        val transport = FakeTransport(err(400, "bad_request"))
        transport.then({ request ->
            val key = org.json.JSONObject(request.body!!).getJSONObject("publicKey")
            answer(Fingerprint.of(EcPublicJwk(key.getString("x"), key.getString("y"))))(request)
        })
        val time = FakeTime()
        Pairing(client(transport, time), keys, info, time.sleep, time.clock, Dispatchers.Unconfined).begin()
        assertTrue(transport.body(0).has("attestation"))
        assertFalse(transport.body(1).has("attestation"))
    }
}

class DeviceSessionTest {
    private val deviceId = "dev_0123456789abcdef"

    private fun challenge(nonce: String) = ok("""{"nonce":"$nonce","expiresIn":60}""")
    private fun token(t: String, expiresIn: Int = 3600) = ok("""{"token":"$t","expiresIn":$expiresIn}""")

    @Test
    fun opensASignedSessionAndReusesTheToken() = runBlocking {
        val key = SoftwareKey()
        val time = FakeTime()
        val transport = FakeTransport(challenge("bm9uY2U"), token("std_a"), ok("{}"), ok("{}"))
        val session = DeviceSession(client(transport, time), deviceId, key, time.clock, Dispatchers.Unconfined)
        val c = client(transport, time)
        session.call { t -> c.health(); assertEquals("std_a", t) }
        session.call { t -> c.health(); assertEquals("std_a", t) }
        assertEquals(listOf("POST /session/challenge", "POST /session", "GET /health", "GET /health"), transport.paths())

        assertEquals(deviceId, transport.body(0).getString("deviceId"))
        val proof = transport.body(1)
        assertEquals(setOf("deviceId", "nonce", "signature"), proof.fieldNames())
        assertEquals("bm9uY2U", proof.getString("nonce"))
        assertTrue(verifies(key.publicJwk(), "stormo-device-session\n$deviceId\nbm9uY2U", proof.getString("signature")))
    }

    @Test
    fun anUnauthorizedCallReopensTheSessionOnce() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(
            challenge("n1"), token("std_a"),
            err(401, "unauthorized"),
            challenge("n2"), token("std_b"),
            ok("""{"agents":[]}"""),
        )
        val c = client(transport, time)
        val session = DeviceSession(c, deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined)
        val seen = mutableListOf<String>()
        session.call { t -> seen += t; c.fleet(t) }
        assertEquals(listOf("std_a", "std_b"), seen)
    }

    @Test
    fun aSecondUnauthorizedReachesTheCaller() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(
            challenge("n1"), token("std_a"), err(401, "unauthorized"),
            challenge("n2"), token("std_b"), err(401, "unauthorized"),
        )
        val c = client(transport, time)
        val session = DeviceSession(c, deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined)
        assertEquals(ErrorCode.UNAUTHORIZED, expect<GatewayException> { session.call { t -> c.fleet(t) } }.code)
    }

    @Test
    fun anExpiredNonceAsksForANewChallenge() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(challenge("n1"), err(400, "expired_nonce"), challenge("n2"), token("std_a"), ok("{}"))
        val c = client(transport, time)
        val session = DeviceSession(c, deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined)
        session.call { c.health() }
        assertEquals("n2", transport.body(3).getString("nonce"))
    }

    @Test
    fun theTokenIsRenewedBeforeItExpires() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(challenge("n1"), token("std_a", 3600), ok("{}"), challenge("n2"), token("std_b"), ok("{}"))
        val c = client(transport, time)
        val session = DeviceSession(c, deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined)
        session.call { c.health() }
        time.now += 3_550_000L // inside the last minute of the token's hour
        val seen = mutableListOf<String>()
        session.call { t -> seen += t; c.health() }
        assertEquals(listOf("std_b"), seen)
    }

    @Test
    fun aChallengeForAnUnlinkedDeviceUnlinksWithoutSigning() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(err(401, "device_unknown"))
        var signed = 0
        val key = object : DeviceKey by SoftwareKey() {
            override fun sign(data: ByteArray): ByteArray {
                signed++
                throw AssertionError("nothing to sign for an unlinked device")
            }
        }
        var unlinked = 0
        val session = DeviceSession(client(transport, time), deviceId, key, time.clock, Dispatchers.Unconfined, onDeviceUnknown = { unlinked++ })
        var ran = false
        assertEquals(ErrorCode.DEVICE_UNKNOWN, expect<GatewayException> { session.call { ran = true } }.code)
        assertEquals(listOf("POST /session/challenge"), transport.paths())
        assertEquals(0, signed)
        assertFalse(ran)
        assertEquals(1, unlinked)
    }

    @Test
    fun aDeviceRevokedMidSessionUnlinksToo() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(challenge("n1"), token("std_a"), err(401, "device_unknown"))
        val c = client(transport, time)
        var unlinked = 0
        val session = DeviceSession(c, deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined, onDeviceUnknown = { unlinked++ })
        expect<GatewayException> { session.call { t -> c.fleet(t) } }
        assertEquals(1, unlinked)

        // Other failures leave the link alone.
        val other = FakeTransport(challenge("n1"), token("std_a"), err(403, "forbidden"))
        val c2 = client(other, time)
        val s2 = DeviceSession(c2, deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined, onDeviceUnknown = { unlinked++ })
        expect<GatewayException> { s2.call { t -> c2.fleet(t) } }
        assertEquals(1, unlinked)
    }

    @Test
    fun anUnknownDeviceOrBadSignatureReachesTheCaller() = runBlocking {
        val time = FakeTime()
        val unknown = FakeTransport(err(401, "device_unknown"))
        val s1 = DeviceSession(client(unknown, time), deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined)
        assertEquals(ErrorCode.DEVICE_UNKNOWN, expect<GatewayException> { s1.call { } }.code)

        val badSig = FakeTransport(challenge("n1"), err(401, "bad_signature"))
        val s2 = DeviceSession(client(badSig, time), deviceId, SoftwareKey(), time.clock, Dispatchers.Unconfined)
        assertEquals(ErrorCode.BAD_SIGNATURE, expect<GatewayException> { s2.call { } }.code)
    }
}

class ReviewTest {
    private val lesson = ReviewItem("support", "Ada", "lesson", "L-7f3a", "9c1d0e7a2b3f4a56", "Answer refund questions with the policy link", AiVerdict(true, "Matches the policy"), appOnly = false, why = null)
    private val skill = ReviewItem("support", "Ada", "skill", "refunds", null, "New skill refunds (3 files)", null, appOnly = true, why = "skills are decided in the app")

    @Test
    fun parsesItemsAndTreatsAMissingFlagAsAppOnly() = runBlocking {
        val transport = FakeTransport(
            ok(
                """{"items":[
                  {"agent":"support","agentName":"Ada","kind":"lesson","id":"L-7f3a","version":"9c1d0e7a2b3f4a56","summary":"s","ai":{"approve":true,"reason":"r"},"appOnly":false},
                  {"agent":"support","agentName":"Ada","kind":"skill","id":"refunds","summary":"s","ai":null,"appOnly":true,"why":"skills are decided in the app"},
                  {"agent":"support","agentName":"Ada","kind":"setting","id":"tone","summary":"s"},
                  {"agent":"support","agentName":"Ada","kind":"setting","id":"style","summary":"s","appOnly":false}
                ]}""",
            ),
        )
        val items = client(transport).review("t")
        // The last one says appOnly: false but carries no version: the watch can't name what it showed.
        assertEquals(listOf(true, false, false, false), items.map { Review.decidableOnWatch(it) })
        assertEquals("9c1d0e7a2b3f4a56", items[0].version)
        assertEquals(null, items[1].version)
        assertEquals("skills are decided in the app", items[1].why)
        assertEquals(null, items[1].ai)
    }

    @Test
    fun anAppOnlyItemIsNeverSent() = runBlocking {
        val transport = FakeTransport()
        val c = client(transport)
        val result = Review.decide(skill, Decision.ACCEPT) { i, d -> c.decide("t", i, d) }
        assertEquals(DecideResult.AppOnly("skills are decided in the app"), result)
        assertTrue(transport.requests.isEmpty())
    }

    @Test
    fun aDecisionIsPostedAsTheContractSays() = runBlocking {
        val transport = FakeTransport(ok("""{"ok":true}"""))
        val c = client(transport)
        assertEquals(DecideResult.Done, Review.decide(lesson, Decision.REJECT) { i, d -> c.decide("t", i, d) })
        assertEquals(listOf("POST /review"), transport.paths())
        val body = transport.body(0)
        assertEquals(setOf("agent", "kind", "id", "version", "decision"), body.fieldNames())
        assertEquals("support", body.getString("agent"))
        assertEquals("9c1d0e7a2b3f4a56", body.getString("version"))
        assertEquals("lesson", body.getString("kind"))
        assertEquals("L-7f3a", body.getString("id"))
        assertEquals("reject", body.getString("decision"))
    }

    @Test
    fun theGatewaysAnswersBecomeResults() = runBlocking {
        val appOnly = FakeTransport(err(400, "app_only"))
        val c1 = client(appOnly)
        assertTrue(Review.decide(lesson, Decision.ACCEPT) { i, d -> c1.decide("t", i, d) } is DecideResult.AppOnly)

        val gone = FakeTransport(err(404, "not_found"))
        val c2 = client(gone)
        assertEquals(DecideResult.Gone, Review.decide(lesson, Decision.ACCEPT) { i, d -> c2.decide("t", i, d) })

        val changed = FakeTransport(err(409, "changed"))
        val c4 = client(changed)
        assertEquals(DecideResult.Changed, Review.decide(lesson, Decision.ACCEPT) { i, d -> c4.decide("t", i, d) })
        assertEquals("This changed; reload.", ErrorText.of(GatewayException(409, ErrorCode.CHANGED, "changed", null, null)))

        val forbidden = FakeTransport(err(403, "forbidden"))
        val c3 = client(forbidden)
        assertEquals(ErrorCode.FORBIDDEN, expect<GatewayException> { Review.decide(lesson, Decision.ACCEPT) { i, d -> c3.decide("t", i, d) } }.code)
    }

    @Test
    fun anItemWithoutAVersionIsNeverSent() = runBlocking {
        val transport = FakeTransport()
        val c = client(transport)
        val unversioned = lesson.copy(version = null)
        assertTrue(Review.decide(unversioned, Decision.ACCEPT) { i, d -> c.decide("t", i, d) } is DecideResult.AppOnly)
        assertTrue(transport.requests.isEmpty())
    }

    @Test
    fun theVerdictLine() {
        assertEquals("AI: approve. Matches the policy", Review.verdictLine(lesson))
        assertEquals(null, Review.verdictLine(skill))
        assertEquals("AI: don't approve", Review.verdictLine(lesson.copy(ai = AiVerdict(false, null))))
    }
}

class ChatPollTest {
    private fun running() = ok("""{"turn":"trn_1","state":"running","conversation":"c1"}""")

    @Test
    fun pollsEveryThreeSecondsUntilDone() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(running(), running(), ok("""{"turn":"trn_1","state":"done","conversation":"c1","reply":"Refunded."}"""))
        val c = client(transport, time)
        val status = Chat.await(fetch = { c.turn("t", "trn_1") }, sleep = time.sleep, clock = time.clock)
        assertEquals("Refunded.", status.reply)
        assertEquals(listOf(3_000L, 3_000L, 3_000L), time.sleeps)
        assertEquals(List(3) { "GET /turns/trn_1" }, transport.paths())
    }

    @Test
    fun shortOutagesAreRiddenOutButAGoneTurnIsNot() = runBlocking {
        val time = FakeTime()
        val flaky = FakeTransport(offline(), err(503, "core_unreachable"), ok("""{"turn":"trn_1","state":"failed","error":"stopped"}"""))
        val c = client(flaky, time)
        assertEquals(TurnState.FAILED, Chat.await(fetch = { c.turn("t", "trn_1") }, sleep = time.sleep, clock = time.clock).state)

        val gone = FakeTransport(err(404, "not_found"))
        val c2 = client(gone, time)
        assertEquals(ErrorCode.NOT_FOUND, expect<GatewayException> { Chat.await(fetch = { c2.turn("t", "trn_1") }, sleep = time.sleep, clock = time.clock) }.code)
    }

    @Test
    fun givesUpAfterATurnsLongestRun() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport()
        repeat(400) { transport.then(running()) }
        val c = client(transport, time)
        expect<ProtocolException> { Chat.await(fetch = { c.turn("t", "trn_1") }, sleep = time.sleep, clock = time.clock) }
        assertTrue(time.sleeps.sum() >= Chat.GIVE_UP_MILLIS)
    }
}
