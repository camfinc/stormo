package stormo.wear.data

import android.app.KeyguardManager
import android.content.Context
import android.os.Build
import androidx.wear.tiles.TileService
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import stormo.wear.BuildConfig
import stormo.wear.core.Chat
import stormo.wear.core.Conversation
import stormo.wear.core.ConversationSummary
import stormo.wear.core.DecideResult
import stormo.wear.core.Decision
import stormo.wear.core.DeviceInfo
import stormo.wear.core.DeviceKeys
import stormo.wear.core.DeviceSession
import stormo.wear.core.ErrorText
import stormo.wear.core.Fleet
import stormo.wear.core.FleetSummary
import stormo.wear.core.GatewayClient
import stormo.wear.core.KeyLostException
import stormo.wear.core.KeyUnavailableException
import stormo.wear.core.Linked
import stormo.wear.core.Me
import stormo.wear.core.NotLinkedException
import stormo.wear.core.Pairing
import stormo.wear.core.Review
import stormo.wear.core.ReviewItem
import stormo.wear.core.StormoException
import stormo.wear.core.TurnStarted
import stormo.wear.core.TurnState
import stormo.wear.core.UrlConnectionTransport
import stormo.wear.tile.FleetTileService

/** Where the watch stands. */
sealed interface LinkState {
    data object NeedsGateway : LinkState
    data object NeedsPairing : LinkState
    data class Linked(val deviceId: String, val name: String) : LinkState
}

/** The one chat turn this watch may have running, followed until it ends. */
data class ActiveTurn(
    val agent: String,
    val conversation: String,
    val turn: String,
    val state: TurnState,
    val reply: String?,
    val error: String?,
)

/**
 * The app's single gateway connection: the stored link, the in-memory session, and every call the
 * screens and the tile make. Nothing authenticated is sent while the watch is locked. A
 * `device_unknown` answer anywhere, or a key that can never sign again, unlinks the watch locally.
 */
class Gateway(private val context: Context) {
    val store = LocalStore(context)
    private val keyguard: KeyguardManager? = context.getSystemService(KeyguardManager::class.java)
    private val keys: DeviceKeys = KeystoreDeviceKeys(isLocked = ::isLocked, isDeviceSecure = ::isDeviceSecure)
    private val transport = UrlConnectionTransport()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)

    val deviceInfo = DeviceInfo.of(
        model = Build.MODEL.orEmpty(),
        sdkInt = Build.VERSION.SDK_INT,
        release = Build.VERSION.RELEASE.orEmpty(),
        appVersion = BuildConfig.VERSION_NAME,
    )

    /** Debug builds may talk cleartext to the emulator host; release builds never do. */
    val allowLocalCleartext: Boolean = BuildConfig.DEBUG
    val defaultGatewayUrl: String = BuildConfig.DEFAULT_GATEWAY_URL

    private val linkState = MutableStateFlow(readLink())
    val link: StateFlow<LinkState> = linkState.asStateFlow()

    private val meState = MutableStateFlow<Me?>(null)
    val me: StateFlow<Me?> = meState.asStateFlow()

    private val turnState = MutableStateFlow<ActiveTurn?>(null)
    val activeTurn: StateFlow<ActiveTurn?> = turnState.asStateFlow()

    /** Why the watch was unlinked without being asked to (revoked, key lost), until it pairs again. */
    private val unlinkedState = MutableStateFlow<String?>(null)
    val unlinkedBecause: StateFlow<String?> = unlinkedState.asStateFlow()

    /** Agent names from the last fleet read, for titles. */
    private val agentNames = HashMap<String, String>()

    @Volatile private var cachedClient: GatewayClient? = null
    @Volatile private var cachedSession: DeviceSession? = null

    val gatewayUrl: String? get() = store.gatewayUrl

    fun agentName(id: String): String = agentNames[id] ?: id

    /** The watch is locked now: the key can't sign and nothing authenticated is sent. */
    fun isLocked(): Boolean = keyguard?.isDeviceLocked == true

    private fun isDeviceSecure(): Boolean = keyguard?.isDeviceSecure == true

    fun client(url: String): GatewayClient {
        cachedClient?.takeIf { it.baseUrl == url.trimEnd('/') }?.let { return it }
        return GatewayClient(url, transport).also { cachedClient = it }
    }

    private fun client(): GatewayClient =
        client(store.gatewayUrl ?: throw NotLinkedException("no gateway address yet"))

    fun setGatewayUrl(url: String) {
        store.gatewayUrl = url
        cachedSession = null
        linkState.value = readLink()
    }

    fun pairing(): Pairing = Pairing(client(), keys, deviceInfo, sleep = { delay(it) })

    fun completePairing(linked: Linked) {
        store.saveLink(linked)
        cachedSession = null
        unlinkedState.value = null
        linkState.value = readLink()
    }

    /** "Unlink this watch": deletes the key and everything stored, the gateway address included. */
    fun unlink() {
        deleteKeyQuietly()
        store.clear(everything = true)
        unlinkedState.value = null
        reset()
    }

    /**
     * The link is gone (the core no longer knows this watch, or its key can never sign again): drop
     * it, keep the address, and say why on the pairing screen.
     */
    private fun forgetLink(reason: StormoException) {
        deleteKeyQuietly()
        store.clear(everything = false)
        unlinkedState.value = ErrorText.of(reason)
        reset()
    }

    private fun reset() {
        cachedSession = null
        meState.value = null
        turnState.value = null
        agentNames.clear()
        linkState.value = readLink()
        requestTileUpdate()
    }

    suspend fun refreshMe(): Me = authed { c, t -> c.me(t) }.also { me ->
        meState.value = me
        val linked = store.linked
        if (linked != null && linked.name != me.name) {
            store.saveName(me.name)
            linkState.value = readLink()
        }
    }

    suspend fun fleet(notifyTile: Boolean = true): Fleet = authed { c, t -> c.fleet(t) }.also { fleet ->
        fleet.agents.forEach { agentNames[it.id] = it.name }
        store.saveSummary(FleetSummary.of(fleet), System.currentTimeMillis())
        if (notifyTile) requestTileUpdate()
    }

    suspend fun review(): List<ReviewItem> = authed { c, t -> c.review(t) }

    suspend fun decide(item: ReviewItem, decision: Decision): DecideResult =
        Review.decide(item, decision) { i, d -> authed { c, t -> c.decide(t, i, d) } }

    suspend fun conversations(agent: String): List<ConversationSummary> = authed { c, t -> c.conversations(t, agent) }

    suspend fun conversation(agent: String, id: String): Conversation = authed { c, t -> c.conversation(t, agent, id) }

    /** Starts a turn and follows it in the background, so leaving the screen doesn't lose it. */
    suspend fun send(agent: String, conversation: String?, message: String): TurnStarted {
        val started = authed { c, t -> c.chat(t, agent, conversation, message) }
        turnState.value = ActiveTurn(agent, started.conversation, started.turn, TurnState.RUNNING, null, null)
        scope.launch { follow(agent, started) }
        return started
    }

    private suspend fun follow(agent: String, started: TurnStarted) {
        val final = try {
            val status = Chat.await(fetch = { authed { c, t -> c.turn(t, started.turn) } }, sleep = { delay(it) })
            ActiveTurn(agent, started.conversation, started.turn, status.state, status.reply, status.error)
        } catch (e: CancellationException) {
            throw e
        } catch (e: StormoException) {
            ActiveTurn(agent, started.conversation, started.turn, TurnState.FAILED, null, ErrorText.of(e))
        }
        if (turnState.value?.turn == started.turn) turnState.value = final
    }

    fun requestTileUpdate() {
        TileService.getUpdater(context).requestUpdate(FleetTileService::class.java)
    }

    /** Every call on behalf of the linked device goes through here. */
    private suspend fun <T> authed(block: suspend (GatewayClient, String) -> T): T {
        val session = session()
        val client = client()
        return session.call { token -> block(client, token) }
    }

    private fun session(): DeviceSession {
        cachedSession?.let { return it }
        val linked = store.linked ?: throw NotLinkedException("this watch isn't linked")
        val key = try {
            keys.load()
        } catch (e: KeyLostException) {
            forgetLink(e)
            throw e
        }
        if (key == null) {
            // The key is gone (the watch was reset): the link can never sign again.
            val lost = KeyLostException(screenLockMissing = !isDeviceSecure())
            forgetLink(lost)
            throw lost
        }
        return DeviceSession(client(), linked.deviceId, key, isLocked = ::isLocked, onLinkLost = ::forgetLink)
            .also { cachedSession = it }
    }

    private fun deleteKeyQuietly() {
        try {
            keys.delete()
        } catch (_: KeyUnavailableException) {
            // Nothing left to delete.
        }
    }

    private fun readLink(): LinkState {
        if (store.gatewayUrl == null) return LinkState.NeedsGateway
        val linked = store.linked ?: return LinkState.NeedsPairing
        return LinkState.Linked(linked.deviceId, linked.name)
    }
}
