package stormo.wear

import android.app.Application
import stormo.wear.data.Gateway

class StormoApp : Application() {
    /** One gateway connection for the app and its tile: the session token lives only in here. */
    val gateway: Gateway by lazy { Gateway(this) }
}
