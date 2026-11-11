package stormo.wear.core

import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertThrows
import org.junit.Assert.assertTrue
import org.junit.Test
import java.math.BigInteger

class Base64UrlTest {
    @Test
    fun encodesWithoutPaddingInTheUrlAlphabet() {
        assertEquals("Zg", Base64Url.encode("f".toByteArray()))
        assertEquals("Zm8", Base64Url.encode("fo".toByteArray()))
        assertEquals("Zm9v", Base64Url.encode("foo".toByteArray()))
        assertEquals("-_8", Base64Url.encode(byteArrayOf(0xFB.toByte(), 0xFF.toByte())))
        assertEquals("", Base64Url.encode(ByteArray(0)))
    }

    @Test
    fun decodesWithOrWithoutPadding() {
        assertArrayEquals("fo".toByteArray(), Base64Url.decode("Zm8"))
        assertArrayEquals("fo".toByteArray(), Base64Url.decode("Zm8="))
        assertArrayEquals(byteArrayOf(0xFB.toByte(), 0xFF.toByte()), Base64Url.decode("-_8"))
    }

    @Test
    fun roundTripsEveryLength() {
        for (n in 0..70) {
            val bytes = ByteArray(n) { (it * 37 + 11).toByte() }
            val text = Base64Url.encode(bytes)
            assertFalse(text.contains('=') || text.contains('+') || text.contains('/'))
            assertArrayEquals(bytes, Base64Url.decode(text))
        }
    }

    @Test
    fun refusesTheStandardAlphabet() {
        assertThrows(IllegalArgumentException::class.java) { Base64Url.decode("+/8") }
    }
}

class JwkTest {
    @Test
    fun aFullWidthCoordinateWithTheTopBitSetDropsItsSignByte() {
        val value = BigInteger(1, ByteArray(32) { 0xFF.toByte() })
        assertEquals(33, value.toByteArray().size)
        val out = EcPublicJwk.coordinate(value)
        assertEquals(32, out.size)
        assertTrue(out.all { it == 0xFF.toByte() })
    }

    @Test
    fun aShortCoordinateIsLeftPadded() {
        val out = EcPublicJwk.coordinate(BigInteger.ONE)
        assertEquals(32, out.size)
        assertTrue(out.take(31).all { it == 0.toByte() })
        assertEquals(1.toByte(), out[31])

        val leadingZeroByte = ByteArray(32) { if (it == 0) 0 else 0x42 }
        assertArrayEquals(leadingZeroByte, EcPublicJwk.coordinate(BigInteger(1, leadingZeroByte)))
        assertEquals(31, BigInteger(1, leadingZeroByte).toByteArray().size)
    }

    @Test
    fun anExactCoordinateIsKept() {
        val bytes = ByteArray(32) { (it + 1).toByte() }
        assertArrayEquals(bytes, EcPublicJwk.coordinate(BigInteger(1, bytes)))
    }

    @Test
    fun refusesCoordinatesThatDoNotFit() {
        assertThrows(IllegalArgumentException::class.java) {
            EcPublicJwk.coordinate(BigInteger.ONE.shiftLeft(256))
        }
        assertThrows(IllegalArgumentException::class.java) { EcPublicJwk.coordinate(BigInteger.ONE.negate()) }
    }

    @Test
    fun aRealKeyEncodesToTwo32ByteCoordinatesThatRebuildIt() {
        repeat(20) {
            val key = SoftwareKey()
            val jwk = key.publicJwk()
            assertEquals(32, Base64Url.decode(jwk.x).size)
            assertEquals(32, Base64Url.decode(jwk.y).size)
            assertEquals(43, jwk.x.length)
            assertEquals(key.pair.public, publicKeyOf(jwk))
        }
    }

    @Test
    fun thumbprintInputIsInLexicographicOrderWithoutSpaces() {
        assertEquals(
            """{"crv":"P-256","kty":"EC","x":"AA","y":"BB"}""",
            EcPublicJwk("AA", "BB").thumbprintInput(),
        )
    }
}

class FingerprintTest {
    private val vector = EcPublicJwk.fromCoordinates(
        BigInteger(1, ByteArray(32) { (it + 1).toByte() }),
        BigInteger(1, ByteArray(32) { (it + 33).toByte() }),
    )

    @Test
    fun matchesTheContractsTestVector() {
        assertEquals("AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA", vector.x)
        assertEquals("ISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0-P0A", vector.y)
        assertEquals(
            """{"crv":"P-256","kty":"EC","x":"AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA","y":"ISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0-P0A"}""",
            vector.thumbprintInput(),
        )
        assertEquals("B756-48F2-D3AD", Fingerprint.of(vector))
    }

    @Test
    fun comparesIgnoringCaseAndGrouping() {
        assertTrue(Fingerprint.matches("B756-48F2-D3AD", "b75648f2d3ad"))
        assertFalse(Fingerprint.matches("B756-48F2-D3AD", "B756-48F2-D3AE"))
        assertFalse(Fingerprint.matches("", ""))
    }

    @Test
    fun differentKeysHaveDifferentFingerprints() {
        val a = Fingerprint.of(SoftwareKey().publicJwk())
        val b = Fingerprint.of(SoftwareKey().publicJwk())
        assertTrue(Regex("[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}").matches(a))
        assertFalse(a == b)
    }
}

class SessionProofTest {
    @Test
    fun theSignedStringIsThreeLinesWithoutATrailingNewline() {
        assertEquals(
            "stormo-device-session\ndev_0123456789abcdef\nbm9uY2U",
            SessionProof.message("dev_0123456789abcdef", "bm9uY2U"),
        )
    }

    @Test
    fun theSignatureIsDerInBase64UrlAndVerifiesWithTheSentKey() {
        val key = SoftwareKey()
        val signature = SessionProof.sign(key, "dev_0123456789abcdef", "bm9uY2U")
        assertFalse(signature.contains('='))
        assertEquals(0x30.toByte(), Base64Url.decode(signature)[0]) // an ASN.1 SEQUENCE
        val jwk = key.publicJwk()
        assertTrue(verifies(jwk, "stormo-device-session\ndev_0123456789abcdef\nbm9uY2U", signature))
        assertFalse(verifies(jwk, "stormo-device-session\ndev_0123456789abcdef\nother", signature))
        assertFalse(verifies(jwk, "stormo-device-session\ndev_0123456789abcdef\nbm9uY2U\n", signature))
    }
}

class SmallHelpersTest {
    @Test
    fun userCodesAreShownInTwoGroups() {
        assertEquals("WDJB-MJHT", UserCode.display("WDJB-MJHT"))
        assertEquals("WDJB-MJHT", UserCode.display("wdjbmjht"))
        assertEquals("WDJB-MJHT", UserCode.display(" wdjb mjht "))
        assertEquals("ABC", UserCode.display("abc"))
        assertEquals("W D J B M J H T", UserCode.spoken("WDJB-MJHT"))
    }

    @Test
    fun attestationIsStandardBase64AndDroppedWhenItDoesNotFit() {
        assertEquals(listOf("AQID"), Attestation.encode(listOf(byteArrayOf(1, 2, 3))))
        assertEquals(listOf("+/8="), Attestation.encode(listOf(byteArrayOf(0xFB.toByte(), 0xFF.toByte()))))
        assertTrue(Attestation.encode(emptyList()).isEmpty())
        assertTrue(Attestation.encode(List(9) { byteArrayOf(1) }).isEmpty())
        assertTrue(Attestation.encode(listOf(ByteArray(8 * 1024 + 1))).isEmpty())
        assertEquals(8, Attestation.encode(List(8) { ByteArray(8 * 1024) }).size)
    }

    @Test
    fun gatewayUrlsAreHttpsOnly() {
        fun ok(input: String, local: Boolean = false) =
            (GatewayUrl.normalize(input, local) as? GatewayUrl.Result.Ok)?.url
        assertEquals("https://gw.example", ok("gw.example"))
        assertEquals("https://gw.example", ok(" https://GW.example/ "))
        assertEquals("https://gw.example:8443/fleet", ok("https://gw.example:8443/fleet/device/v1/"))
        assertEquals(null, ok("http://gw.example"))
        assertEquals(null, ok("http://gw.example", local = true))
        assertEquals("http://10.0.2.2:18650", ok("http://10.0.2.2:18650", local = true))
        assertEquals(null, ok("http://10.0.2.2:18650"))
        assertEquals(null, ok("https://user:pw@gw.example"))
        assertEquals(null, ok("https://gw.example/?x=1"))
        assertEquals(null, ok("ftp://gw.example"))
        assertEquals(null, ok(""))
    }

    @Test
    fun deviceInfoNamesWearOsAndFitsTheContract() {
        val info = DeviceInfo.of("Pixel Watch 3", 36, "16", "0.1.0")
        assertEquals(DeviceInfo("Watch", "Pixel Watch 3", "Wear OS 6", "0.1.0"), info)
        assertEquals("Wear OS 5", DeviceInfo.wearOsName(34, "14"))
        assertEquals("Wear OS (Android 17)", DeviceInfo.wearOsName(37, "17"))
        val long = DeviceInfo.of("M".repeat(100), 30, "11", "1".repeat(40))
        assertEquals(64, long.model.length)
        assertEquals(32, long.appVersion.length)
        assertEquals("Wear OS watch", DeviceInfo.of("  ", 30, "11", "1").model)
    }

    @Test
    fun theTileCountsAndTheAgentLine() {
        val fleet = Fleet(
            listOf(
                FleetAgent("support", "Ada", "ops", "running", true, "web_search", 1, 2, 3),
                FleetAgent("billing", "Bea", "ops", "stopped", false, null, 0, 0, 1),
            ),
            null,
        )
        assertEquals(FleetSummary(agents = 2, running = 1, busy = 1, waiting = 1, pendingReview = 4), FleetSummary.of(fleet))
        assertEquals("Running · web_search · 2 unread · 1 waiting · 3 to review", AgentText.status(fleet.agents[0]))
        assertEquals("Stopped · 1 to review", AgentText.status(fleet.agents[1]))
    }
}
