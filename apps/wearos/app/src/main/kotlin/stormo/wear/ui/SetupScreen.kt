package stormo.wear.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.wear.compose.material3.Button
import androidx.wear.compose.material3.EdgeButton
import androidx.wear.compose.material3.ListHeader
import androidx.wear.compose.material3.Text
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import stormo.wear.core.ErrorText
import stormo.wear.core.GatewayUrl
import stormo.wear.data.Gateway

class SetupViewModel(private val gateway: Gateway) : ViewModel() {
    data class State(val url: String?, val error: String? = null, val checking: Boolean = false)

    private val state = MutableStateFlow(State(initialUrl()))
    val ui: StateFlow<State> = state.asStateFlow()

    private fun initialUrl(): String? {
        val stored = gateway.gatewayUrl ?: gateway.defaultGatewayUrl
        return (GatewayUrl.normalize(stored, gateway.allowLocalCleartext) as? GatewayUrl.Result.Ok)?.url
    }

    fun edit(input: String) {
        when (val result = GatewayUrl.normalize(input, gateway.allowLocalCleartext)) {
            is GatewayUrl.Result.Ok -> state.value = State(result.url)
            is GatewayUrl.Result.Invalid -> state.update { it.copy(error = result.reason) }
        }
    }

    fun confirm(onDone: () -> Unit) {
        val url = state.value.url ?: return
        if (state.value.checking) return
        state.update { it.copy(checking = true, error = null) }
        viewModelScope.launch {
            attempt { gateway.client(url).health() }
                .onSuccess { healthy ->
                    if (healthy) {
                        gateway.setGatewayUrl(url)
                        state.update { it.copy(checking = false) }
                        onDone()
                    } else {
                        state.update { it.copy(checking = false, error = "That address doesn't answer like a gateway.") }
                    }
                }
                .onFailure { e -> state.update { it.copy(checking = false, error = ErrorText.of(e)) } }
        }
    }
}

@Composable
fun SetupScreen(gateway: Gateway, onDone: () -> Unit) {
    val vm = viewModel { SetupViewModel(gateway) }
    val ui by vm.ui.collectAsStateWithLifecycle()
    val editUrl = rememberTextInput("Gateway address") { vm.edit(it) }

    ListScreen(
        edgeButton = {
            EdgeButton(onClick = { vm.confirm(onDone) }, enabled = ui.url != null && !ui.checking) {
                Text(if (ui.checking) "Checking…" else "Continue")
            }
        },
    ) { list ->
        item { ListHeader(modifier = itemModifier(list), transformation = transformation(list)) { Text("Stormo") } }
        item { Note("Where does this watch reach your fleet?", itemModifier(list)) }
        item {
            Button(
                onClick = editUrl,
                modifier = itemModifier(list),
                transformation = transformation(list),
                label = { Text("Gateway") },
                secondaryLabel = { Text(ui.url ?: "Tap to enter", maxLines = 3) },
            )
        }
        ui.error?.let { error -> item { Note(error, itemModifier(list), error = true) } }
        if (ui.checking) item { InlineBusy("Reaching the gateway…", itemModifier(list)) }
    }
}
