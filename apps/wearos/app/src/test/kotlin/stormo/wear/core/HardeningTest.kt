package stormo.wear.core

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertSame
import org.junit.Assert.assertTrue
import org.junit.Test

private const val DEVICE = "dev_0123456789abcdef"

private fun challenge(nonce: String) = ok("""{"nonce":"$nonce","expiresIn":60}""")
private fun token(t: String) = ok("""{"token":"$t","expiresIn":3600}""")
private fun running() = ok("""{"turn":"trn_1","state":"running","conversation":"c1"}""")
private fun done() = ok("""{"turn":"trn_1","state":"done","conversation":"c1","reply":"Refunded."}""")

class LockedWatchTest {
    @Test
    fun aLockedWatchSendsNothingAndDropsItsToken() = runBlocking {
        val time = FakeTime()
        var locked = false
        val transport = FakeTransport(challenge("n1"), token("std_a"), ok("{}"))
        val c = client(transport, time)
        val session = DeviceSession(c, DEVICE, SoftwareKey(), time.clock, Dispatchers.Unconfined, isLocked = { locked })
        session.call { c.health() }

        locked = true
        var ran = false
        expect<DeviceLockedException> { session.call { ran = true; c.health() } }
        assertFalse("the call itself never runs", ran)
        assertEquals("nothing is sent while locked", 3, transport.requests.size)

        locked = false
        transport.then(challenge("n2"), token("std_b"), ok("{}"))
        val seen = mutableListOf<String>()
        session.call { t -> seen += t; c.health() }
        assertEquals("the old token was dropped at the lock", listOf("std_b"), seen)
    }

    @Test
    fun aLockedWatchNeverOpensASession() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport()
        var signed = 0
        val key = object : DeviceKey by SoftwareKey() {
            override fun sign(data: ByteArray): ByteArray {
                signed++
                return ByteArray(0)
            }
        }
        val session = DeviceSession(client(transport, time), DEVICE, key, time.clock, Dispatchers.Unconfined, isLocked = { true })
        expect<DeviceLockedException> { session.call { } }
        assertTrue(transport.requests.isEmpty())
        assertEquals(0, signed)
    }

    @Test
    fun turnFollowingPausesWhileLockedAndResumes() = runBlocking {
        val time = FakeTime()
        // Locked for 15 minutes, longer than a turn may run: following must not give up for it.
        var lockedPolls = 300
        val status = Chat.await(
            fetch = {
                if (lockedPolls-- > 0) throw DeviceLockedException()
                TurnStatus("trn_1", TurnState.DONE, "c1", "Refunded.", null)
            },
            sleep = time.sleep,
            clock = time.clock,
        )
        assertEquals(TurnState.DONE, status.state)
        assertEquals(301, time.sleeps.size)
    }
}

class SharedRateLimitTest {
    @Test
    fun aLongLimitAnswersEveryRouteWithoutCalling() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(err(429, "rate_limited", retryAfter = "120"))
        val c = client(transport, time)
        expect<GatewayException> { c.fleet("t") }

        val review = expect<GatewayException> { c.review("t") }
        assertEquals(ErrorCode.RATE_LIMITED, review.code)
        assertEquals(120L, review.retryAfterSeconds)
        time.now += 60_000
        assertEquals(60L, c.rateLimitedFor())
        assertEquals(60L, expect<GatewayException> { c.chat("t", "support", null, "Hello") }.retryAfterSeconds)
        assertEquals("only the first call reached the gateway", 1, transport.requests.size)
        assertTrue(time.sleeps.isEmpty())

        // Under 30 s left: the next caller waits it out, then calls.
        time.now += 31_000
        transport.then(ok("""{"agents":[]}"""))
        c.fleet("t")
        assertEquals(listOf(29_000L), time.sleeps)
        assertEquals(2, transport.requests.size)
        assertEquals(0L, c.rateLimitedFor())
    }

    @Test
    fun aShortLimitMakesTheNextCallerWaitToo() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(
            err(429, "rate_limited", retryAfter = "20"),
            err(429, "rate_limited", retryAfter = "20"),
            ok("""{"items":[]}"""),
        )
        val c = client(transport, time)
        expect<GatewayException> { c.fleet("t") } // retried once after 20 s, limited again
        assertEquals(listOf(20_000L), time.sleeps)
        c.review("t") // a different route still waits for the shared limit
        assertEquals(listOf(20_000L, 20_000L), time.sleeps)
        assertEquals(3, transport.requests.size)
    }

    @Test
    fun aLimitWithoutRetryAfterWaitsTheDefault() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport({ HttpResponse(429, emptyMap(), "") }, ok("""{"status":"ok"}"""))
        assertTrue(client(transport, time).health())
        assertEquals(listOf(GatewayClient.DEFAULT_RETRY_AFTER_SECONDS * 1000), time.sleeps)
    }

    @Test
    fun turnFollowingWaitsOutALimitInsteadOfFailing() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport(running(), err(429, "rate_limited", retryAfter = "90"), running(), done())
        val c = client(transport, time)
        val status = Chat.await(fetch = { c.turn("t", "trn_1") }, sleep = time.sleep, clock = time.clock)
        assertEquals(TurnState.DONE, status.state)
        assertEquals(listOf(3_000L, 3_000L, 90_000L, 3_000L, 3_000L), time.sleeps)
        assertEquals(4, transport.requests.size)
    }

    @Test
    fun aContendedReviewIsTryAgainAndBusyStaysChats() = runBlocking {
        val item = ReviewItem("support", "Ada", "lesson", "L-7f3a", "9c1d0e7a2b3f4a56", "s", null, appOnly = false, why = null)
        val limited = FakeTransport(err(429, "rate_limited", retryAfter = "45"))
        val c1 = client(limited)
        assertEquals(DecideResult.TryAgain(45), Review.decide(item, Decision.ACCEPT) { i, d -> c1.decide("t", i, d) })
        assertEquals(1, limited.requests.size)

        // `busy` is chat's ("a reply is already running"): review does not read it as a result.
        val busy = FakeTransport(err(409, "busy"))
        val c2 = client(busy)
        assertEquals(ErrorCode.BUSY, expect<GatewayException> { Review.decide(item, Decision.ACCEPT) { i, d -> c2.decide("t", i, d) } }.code)
        assertTrue(ErrorText.of(GatewayException(409, ErrorCode.BUSY, "busy", null, null)).contains("reply"))
    }
}

class KeyFailureTest {
    @Test
    fun aKeyThatCanNeverSignAgainIsLost() {
        for (cause in listOf(KeyFailureCause.INVALIDATED, KeyFailureCause.UNRECOVERABLE, KeyFailureCause.MISSING)) {
            for (locked in listOf(true, false)) {
                val secure = KeyFailures.classify(cause, deviceLocked = locked, deviceSecure = true)
                assertTrue("$cause", secure is KeyLostException)
                assertFalse((secure as KeyLostException).screenLockMissing)
                val noLock = KeyFailures.classify(cause, deviceLocked = locked, deviceSecure = false) as KeyLostException
                assertTrue(noLock.screenLockMissing)
            }
        }
    }

    @Test
    fun aLockRelatedFailureAsksToUnlock() {
        assertTrue(KeyFailures.classify(KeyFailureCause.USER_NOT_AUTHENTICATED, deviceLocked = false, deviceSecure = true) is DeviceLockedException)
        assertTrue(KeyFailures.classify(KeyFailureCause.OTHER, deviceLocked = true, deviceSecure = true) is DeviceLockedException)
        assertTrue(KeyFailures.classify(KeyFailureCause.OTHER, deviceLocked = false, deviceSecure = true) is KeyUnavailableException)
    }

    @Test
    fun eachFailureSaysWhatToDo() {
        assertEquals("Unlock the watch to reach the fleet.", ErrorText.of(DeviceLockedException()))
        assertTrue(ErrorText.of(KeyLostException(screenLockMissing = true)).contains("Set a screen lock"))
        assertFalse(ErrorText.of(KeyLostException(screenLockMissing = false)).contains("screen lock"))
        assertFalse("not every failure is the lock", ErrorText.of(KeyUnavailableException("x")).contains("Unlock"))
        assertEquals("This watch has no hardware key store; it can't be linked.", ErrorText.of(NoHardwareKeyStoreException()))
    }

    @Test
    fun onlyHardwareKeysBackALink() {
        assertTrue(KeyHardware.isHardwareBacked(KeySecurity.STRONGBOX))
        assertTrue(KeyHardware.isHardwareBacked(KeySecurity.TRUSTED_ENVIRONMENT))
        assertTrue(KeyHardware.isHardwareBacked(KeySecurity.UNKNOWN_SECURE))
        assertFalse(KeyHardware.isHardwareBacked(KeySecurity.SOFTWARE))
        assertFalse(KeyHardware.isHardwareBacked(KeySecurity.UNKNOWN))
    }

    @Test
    fun aWatchWithoutHardwareKeysCantStartAPairing() = runBlocking {
        val time = FakeTime()
        val transport = FakeTransport()
        val softwareOnly = object : DeviceKeys {
            override fun load(): DeviceKey? = null
            override fun create(): CreatedKey = throw NoHardwareKeyStoreException()
            override fun delete() = Unit
        }
        val pairing = Pairing(client(transport, time), softwareOnly, DeviceInfo.of("Pixel Watch 3", 36, "16", "0.1.0"), time.sleep, time.clock, Dispatchers.Unconfined)
        expect<NoHardwareKeyStoreException> { pairing.begin() }
        assertTrue("no pairing is asked for", transport.requests.isEmpty())
    }

    @Test
    fun aKeyLostWhileSigningUnlinks() = runBlocking {
        val time = FakeTime()
        val lost = KeyLostException(screenLockMissing = false)
        val key = object : DeviceKey by SoftwareKey() {
            override fun sign(data: ByteArray): ByteArray = throw lost
        }
        val reasons = mutableListOf<StormoException>()
        val transport = FakeTransport(challenge("n1"))
        val session = DeviceSession(client(transport, time), DEVICE, key, time.clock, Dispatchers.Unconfined, onLinkLost = { reasons += it })
        assertSame(lost, expect<KeyLostException> { session.call { } })
        assertEquals(listOf<StormoException>(lost), reasons)
        assertEquals(listOf("POST /session/challenge"), transport.paths())
    }
}
