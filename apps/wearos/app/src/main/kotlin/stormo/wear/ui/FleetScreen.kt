package stormo.wear.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.wear.compose.foundation.lazy.items
import androidx.wear.compose.material3.Button
import androidx.wear.compose.material3.EdgeButton
import androidx.wear.compose.material3.FilledTonalButton
import androidx.wear.compose.material3.ListHeader
import androidx.wear.compose.material3.OutlinedButton
import androidx.wear.compose.material3.Text
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import stormo.wear.core.AgentText
import stormo.wear.core.ErrorText
import stormo.wear.core.Fleet
import stormo.wear.core.Grants
import stormo.wear.core.Me
import stormo.wear.data.Gateway

class FleetViewModel(private val gateway: Gateway) : ViewModel() {
    data class State(
        val me: Me? = null,
        val fleet: Fleet? = null,
        val loading: Boolean = true,
        val error: String? = null,
    )

    private val state = MutableStateFlow(State(me = gateway.me.value))
    val ui: StateFlow<State> = state.asStateFlow()

    suspend fun refresh() {
        state.update { it.copy(loading = true) }
        attempt {
            val me = gateway.refreshMe()
            me to if (me.grants.status) gateway.fleet() else null
        }.onSuccess { (me, fleet) ->
            state.value = State(me = me, fleet = fleet, loading = false)
        }.onFailure { e ->
            state.update { it.copy(loading = false, error = ErrorText.of(e)) }
        }
    }

    fun reload() {
        viewModelScope.launch { refresh() }
    }
}

@Composable
fun FleetScreen(gateway: Gateway, onAgent: (String) -> Unit, onReview: () -> Unit, onSettings: () -> Unit) {
    val vm = viewModel { FleetViewModel(gateway) }
    val ui by vm.ui.collectAsStateWithLifecycle()
    WhileResumed(periodMillis = 30_000) { vm.refresh() }

    val grants = ui.me?.grants ?: Grants.NONE
    ListScreen(
        edgeButton = {
            EdgeButton(onClick = vm::reload, enabled = !ui.loading) { Text(if (ui.loading) "Loading…" else "Refresh") }
        },
    ) { list ->
        item {
            ListHeader(modifier = itemModifier(list), transformation = transformation(list)) {
                Text(ui.me?.instance ?: "Stormo", maxLines = 1)
            }
        }
        if (grants.review) {
            item {
                val pending = ui.fleet?.agents?.sumOf { it.pendingReview }
                FilledTonalButton(
                    onClick = onReview,
                    modifier = itemModifier(list),
                    transformation = transformation(list),
                    label = { Text("Review") },
                    secondaryLabel = if (pending == null) null else {
                        { Text(if (pending == 1) "1 pending" else "$pending pending") }
                    },
                )
            }
        }
        val fleet = ui.fleet
        when {
            fleet != null -> {
                if (fleet.agents.isEmpty()) item { Note("No agents yet.", itemModifier(list)) }
                items(fleet.agents, key = { it.id }) { agent ->
                    val canChat = grants.canChat(agent.id)
                    Button(
                        onClick = { onAgent(agent.id) },
                        enabled = canChat,
                        modifier = itemModifier(list),
                        transformation = transformation(list),
                        icon = { StateDot(agent.state) },
                        label = { Text(agent.name, maxLines = 1) },
                        secondaryLabel = { Text(AgentText.status(agent), maxLines = 2) },
                    )
                }
            }
            ui.me != null && !grants.status -> {
                // No fleet view: the agents this watch may talk with, by id.
                val named = grants.chat.filter { it != Grants.WILDCARD }.distinct()
                if (named.isEmpty() && !grants.chatsWithEveryAgent) {
                    item { Note("This watch has no fleet or chat access.", itemModifier(list)) }
                } else if (named.isEmpty()) {
                    item { Note("Ask for the status grant to list the agents.", itemModifier(list)) }
                }
                items(named, key = { it }) { id ->
                    Button(
                        onClick = { onAgent(id) },
                        modifier = itemModifier(list),
                        transformation = transformation(list),
                        label = { Text(gateway.agentName(id), maxLines = 1) },
                    )
                }
            }
            ui.loading -> item { Busy("Reading the fleet…", itemModifier(list)) }
        }
        ui.error?.let { error -> item { Note(error, itemModifier(list), error = true) } }
        item {
            OutlinedButton(
                onClick = onSettings,
                modifier = itemModifier(list),
                transformation = transformation(list),
                label = { Text("Settings") },
            )
        }
    }
}
