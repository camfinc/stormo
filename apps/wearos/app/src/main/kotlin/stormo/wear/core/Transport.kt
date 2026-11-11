package stormo.wear.core

import java.io.ByteArrayOutputStream
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URI

data class HttpRequest(
    val method: String,
    val url: String,
    val headers: Map<String, String>,
    val body: String?,
)

/** An HTTP answer; header names are lower case. */
data class HttpResponse(val status: Int, val headers: Map<String, String>, val body: String) {
    fun header(name: String): String? = headers[name.lowercase()]
}

/** One HTTP exchange, blocking. The app uses [UrlConnectionTransport]; tests use a fake. */
fun interface Transport {
    @Throws(IOException::class)
    fun execute(request: HttpRequest): HttpResponse
}

/**
 * [Transport] over [HttpURLConnection]: timeouts on every request, no redirects (a redirect could
 * carry the bearer token elsewhere), no caching, bodies capped so a misbehaving edge can't exhaust
 * the watch's memory. Cleartext is refused by the app's network security config.
 */
class UrlConnectionTransport(
    private val connectTimeoutMillis: Int = 10_000,
    private val readTimeoutMillis: Int = 30_000,
    private val maxBodyBytes: Int = 1 shl 20,
) : Transport {

    override fun execute(request: HttpRequest): HttpResponse {
        val connection = URI(request.url).toURL().openConnection() as HttpURLConnection
        try {
            connection.requestMethod = request.method
            connection.connectTimeout = connectTimeoutMillis
            connection.readTimeout = readTimeoutMillis
            connection.instanceFollowRedirects = false
            connection.useCaches = false
            connection.setRequestProperty("Accept", "application/json")
            for ((name, value) in request.headers) connection.setRequestProperty(name, value)
            if (request.body != null) {
                val bytes = request.body.toByteArray(Charsets.UTF_8)
                connection.doOutput = true
                connection.setRequestProperty("Content-Type", "application/json; charset=utf-8")
                connection.outputStream.use { it.write(bytes) }
            }
            val status = connection.responseCode
            val stream = if (status >= 400) connection.errorStream else connection.inputStream
            val body = stream?.use { read(it) } ?: ""
            val headers = HashMap<String, String>()
            for ((name, values) in connection.headerFields) {
                if (name != null && !values.isNullOrEmpty()) headers[name.lowercase()] = values[0]
            }
            return HttpResponse(status, headers, body)
        } finally {
            connection.disconnect()
        }
    }

    private fun read(input: InputStream): String {
        val out = ByteArrayOutputStream()
        val buffer = ByteArray(8 * 1024)
        while (true) {
            val n = input.read(buffer)
            if (n < 0) break
            if (out.size() + n > maxBodyBytes) throw IOException("answer larger than $maxBodyBytes bytes")
            out.write(buffer, 0, n)
        }
        return out.toString(Charsets.UTF_8.name())
    }
}
