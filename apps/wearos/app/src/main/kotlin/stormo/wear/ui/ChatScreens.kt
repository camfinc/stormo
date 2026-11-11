package stormo.wear.ui

import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.wear.compose.foundation.lazy.items
import androidx.wear.compose.foundation.lazy.rememberTransformingLazyColumnState
import androidx.wear.compose.material3.EdgeButton
import androidx.wear.compose.material3.ListHeader
import androidx.wear.compose.material3.MaterialTheme
import androidx.wear.compose.material3.Text
import androidx.wear.compose.material3.TitleCard
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import stormo.wear.core.Chat
import stormo.wear.core.ChatMessage
import stormo.wear.core.Conversation
import stormo.wear.core.ConversationSummary
import stormo.wear.core.ErrorText
import stormo.wear.core.TimeLabel
import stormo.wear.core.TurnState
import stormo.wear.data.Gateway

// An agent's recent conversations.

class AgentViewModel(private val gateway: Gateway, val agent: String) : ViewModel() {
    data class State(
        val conversations: List<ConversationSummary>? = null,
        val loading: Boolean = true,
        val sending: Boolean = false,
        val error: String? = null,
    )

    private val state = MutableStateFlow(State())
    val ui: StateFlow<State> = state.asStateFlow()

    suspend fun refresh() {
        state.update { it.copy(loading = true) }
        attempt { gateway.conversations(agent) }
            .onSuccess { list -> state.update { it.copy(conversations = list, loading = false, error = null) } }
            .onFailure { e -> state.update { it.copy(loading = false, error = ErrorText.of(e)) } }
    }

    /** Starts a new conversation with [message]; [onStarted] gets its id. */
    fun start(message: String, onStarted: (String) -> Unit) {
        Chat.problem(message)?.let { problem -> state.update { it.copy(error = problem) }; return }
        if (state.value.sending) return
        state.update { it.copy(sending = true, error = null) }
        viewModelScope.launch {
            attempt { gateway.send(agent, null, message) }
                .onSuccess { started ->
                    state.update { it.copy(sending = false) }
                    onStarted(started.conversation)
                }
                .onFailure { e -> state.update { it.copy(sending = false, error = ErrorText.of(e)) } }
        }
    }
}

@Composable
fun AgentScreen(gateway: Gateway, agent: String, onConversation: (String) -> Unit) {
    val vm = viewModel(key = "agent:$agent") { AgentViewModel(gateway, agent) }
    val ui by vm.ui.collectAsStateWithLifecycle()
    WhileResumed(periodMillis = 60_000) { vm.refresh() }
    val ask = rememberTextInput("Message ${gateway.agentName(agent)}") { vm.start(it, onConversation) }

    ListScreen(
        edgeButton = {
            EdgeButton(onClick = ask, enabled = !ui.sending) { Text(if (ui.sending) "Sending…" else "New") }
        },
    ) { list ->
        item {
            ListHeader(modifier = itemModifier(list), transformation = transformation(list)) {
                Text(gateway.agentName(agent), maxLines = 1)
            }
        }
        val conversations = ui.conversations
        when {
            conversations == null && ui.loading -> item { Busy("Reading conversations…", itemModifier(list)) }
            conversations != null && conversations.isEmpty() -> item { Note("No conversations yet. Start one below.", itemModifier(list)) }
            conversations != null -> items(conversations, key = { it.id }) { c ->
                val time = TimeLabel.of(c.lastActive)
                TitleCard(
                    onClick = { onConversation(c.id) },
                    modifier = itemModifier(list),
                    transformation = transformation(list),
                    title = { Text(c.title?.takeIf { it.isNotBlank() } ?: "Conversation", maxLines = 1) },
                    time = if (time == null) null else {
                        { Text(time) }
                    },
                ) {
                    c.preview?.takeIf { it.isNotBlank() }?.let { Text(it, maxLines = 2) }
                }
            }
        }
        ui.error?.let { error -> item { Note(error, itemModifier(list), error = true) } }
    }
}

// One conversation: its messages and a reply.

class ConversationViewModel(private val gateway: Gateway, val agent: String, val id: String) : ViewModel() {
    data class State(
        val conversation: Conversation? = null,
        val loading: Boolean = true,
        val sending: Boolean = false,
        val error: String? = null,
    )

    private val state = MutableStateFlow(State())
    val ui: StateFlow<State> = state.asStateFlow()
    val turn = gateway.activeTurn
    private var reloadedFor: String? = null

    init {
        // A turn of this conversation that just ended: read the conversation again for the reply.
        viewModelScope.launch {
            gateway.activeTurn.collect { t ->
                if (t != null && t.conversation == id && t.state != TurnState.RUNNING && t.turn != reloadedFor) {
                    reloadedFor = t.turn
                    refresh()
                }
            }
        }
    }

    suspend fun refresh() {
        state.update { it.copy(loading = true) }
        attempt { gateway.conversation(agent, id) }
            .onSuccess { c -> state.update { it.copy(conversation = c, loading = false, error = null) } }
            .onFailure { e -> state.update { it.copy(loading = false, error = ErrorText.of(e)) } }
    }

    fun reply(message: String) {
        Chat.problem(message)?.let { problem -> state.update { it.copy(error = problem) }; return }
        if (state.value.sending) return
        state.update { it.copy(sending = true, error = null) }
        viewModelScope.launch {
            attempt { gateway.send(agent, id, message) }
                .onSuccess { state.update { it.copy(sending = false) } }
                .onFailure { e -> state.update { it.copy(sending = false, error = ErrorText.of(e)) } }
            refresh()
        }
    }
}

@Composable
fun ConversationScreen(gateway: Gateway, agent: String, conversation: String) {
    val vm = viewModel(key = "conversation:$agent/$conversation") { ConversationViewModel(gateway, agent, conversation) }
    val ui by vm.ui.collectAsStateWithLifecycle()
    val active by vm.turn.collectAsStateWithLifecycle()
    val turn = active?.takeIf { it.conversation == conversation }
    val replying = turn?.state == TurnState.RUNNING
    val name = gateway.agentName(agent)
    val reply = rememberTextInput("Reply to $name") { vm.reply(it) }
    WhileResumed(periodMillis = 60_000) { vm.refresh() }

    val listState = rememberTransformingLazyColumnState()
    val messages = ui.conversation?.messages.orEmpty()
    // Open at the latest message: the header, the messages, then the status rows.
    LaunchedEffect(messages.size, replying) {
        if (messages.isNotEmpty()) listState.scrollToItem(messages.size)
    }

    ListScreen(
        state = listState,
        edgeButton = {
            EdgeButton(onClick = reply, enabled = !replying && !ui.sending) {
                Text(if (replying || ui.sending) "Wait…" else "Reply")
            }
        },
    ) { list ->
        item { ListHeader(modifier = itemModifier(list), transformation = transformation(list)) { Text(name, maxLines = 1) } }
        items(messages) { message ->
            MessageRow(message, name, itemModifier(list))
        }
        if (ui.conversation?.compacted == true && messages.isNotEmpty()) {
            item { Note("Earlier messages were summarized.", itemModifier(list)) }
        }
        when {
            replying || ui.sending -> item { InlineBusy("$name is replying…", itemModifier(list)) }
            turn?.state == TurnState.FAILED -> item { Note(turn?.error ?: "The reply failed.", itemModifier(list), error = true) }
        }
        if (ui.conversation == null && ui.loading) item { Busy("Reading…", itemModifier(list)) }
        ui.error?.let { error -> item { Note(error, itemModifier(list), error = true) } }
    }
}

@Composable
private fun MessageRow(message: ChatMessage, agentName: String, modifier: Modifier = Modifier) {
    Panel(modifier = modifier.fillMaxWidth(), highlight = message.fromUser) {
        val who = if (message.fromUser) "You" else agentName
        val time = TimeLabel.of(message.at)
        Text(
            if (time == null) who else "$who · $time",
            style = MaterialTheme.typography.labelSmall,
            color = if (message.fromUser) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(
            message.content,
            style = MaterialTheme.typography.bodyMedium,
            color = if (message.fromUser) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurface,
        )
    }
}
