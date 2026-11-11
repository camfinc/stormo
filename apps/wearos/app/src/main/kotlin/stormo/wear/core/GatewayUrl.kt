package stormo.wear.core

import java.net.URI
import java.net.URISyntaxException

/**
 * The gateway's base URL as the watch stores it: `https://host[:port][/prefix]`, no trailing slash.
 * Cleartext `http` is accepted only when [normalize] is told to (debug builds) and only for the
 * emulator's and the machine's loopback addresses, matching the debug network security config.
 */
object GatewayUrl {
    val LOCAL_CLEARTEXT_HOSTS = setOf("10.0.2.2", "localhost", "127.0.0.1")

    sealed interface Result {
        data class Ok(val url: String) : Result
        data class Invalid(val reason: String) : Result
    }

    fun normalize(input: String, allowLocalCleartext: Boolean): Result {
        var text = input.trim()
        if (text.isEmpty()) return Result.Invalid("Enter the gateway's address.")
        if (!text.contains("://")) text = "https://$text"
        val uri = try {
            URI(text)
        } catch (_: URISyntaxException) {
            return Result.Invalid("That isn't a web address.")
        }
        val scheme = uri.scheme?.lowercase()
        val host = uri.host?.lowercase()
        if (host.isNullOrEmpty()) return Result.Invalid("The address needs a host name.")
        if (uri.rawUserInfo != null) return Result.Invalid("Leave out user names and passwords.")
        if (uri.rawQuery != null || uri.rawFragment != null) return Result.Invalid("Leave out ? and # parts.")
        when (scheme) {
            "https" -> Unit
            "http" -> if (!allowLocalCleartext || host !in LOCAL_CLEARTEXT_HOSTS) {
                return Result.Invalid("The gateway must use https.")
            }
            else -> return Result.Invalid("The gateway must use https.")
        }
        // Someone pasting a route keeps only the base.
        val path = (uri.rawPath ?: "").trimEnd('/').removeSuffix("/device/v1").trimEnd('/')
        val port = if (uri.port != -1) ":${uri.port}" else ""
        return Result.Ok("$scheme://$host$port$path")
    }
}

/** What the pairing request says about this watch, clamped to the contract's lengths. */
data class DeviceInfo(val name: String, val model: String, val os: String, val appVersion: String) {
    companion object {
        const val NAME = "Watch"

        fun of(model: String, sdkInt: Int, release: String, appVersion: String) = DeviceInfo(
            name = NAME,
            model = clamp(model.trim().ifEmpty { "Wear OS watch" }, 64),
            os = clamp(wearOsName(sdkInt, release), 32),
            appVersion = clamp(appVersion.ifEmpty { "0" }, 32),
        )

        /** Wear OS's own version for the platform level it runs on. */
        fun wearOsName(sdkInt: Int, release: String): String = when (sdkInt) {
            30 -> "Wear OS 3"
            33 -> "Wear OS 4"
            34 -> "Wear OS 5"
            35 -> "Wear OS 5.1"
            36 -> "Wear OS 6"
            else -> "Wear OS (Android $release)"
        }

        private fun clamp(s: String, max: Int) = if (s.length <= max) s else s.substring(0, max)
    }
}
