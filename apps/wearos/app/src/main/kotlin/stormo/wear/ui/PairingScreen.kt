package stormo.wear.ui

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.wear.compose.material3.EdgeButton
import androidx.wear.compose.material3.ListHeader
import androidx.wear.compose.material3.MaterialTheme
import androidx.wear.compose.material3.OutlinedButton
import androidx.wear.compose.material3.Text
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import stormo.wear.core.ErrorText
import stormo.wear.core.FingerprintMismatchException
import stormo.wear.core.PairingOutcome
import stormo.wear.core.UserCode
import stormo.wear.data.Gateway

sealed interface PairingUi {
    data object Preparing : PairingUi
    data class Code(val userCode: String, val fingerprint: String) : PairingUi
    data object Linked : PairingUi
    data object Expired : PairingUi
    data object Denied : PairingUi
    data class Mismatch(val local: String, val remote: String) : PairingUi
    data class Failed(val message: String) : PairingUi
}

class PairingViewModel(private val gateway: Gateway) : ViewModel() {
    private val state = MutableStateFlow<PairingUi>(PairingUi.Preparing)
    val ui: StateFlow<PairingUi> = state.asStateFlow()
    private var job: Job? = null

    init {
        start()
    }

    /** Starts (or restarts) a pairing with a fresh key. */
    fun start() {
        job?.cancel()
        state.value = PairingUi.Preparing
        job = viewModelScope.launch {
            val pairing = attempt { gateway.pairing() }.getOrElse { e ->
                state.value = PairingUi.Failed(ErrorText.of(e))
                return@launch
            }
            val ticket = attempt { pairing.begin() }.getOrElse { e ->
                state.value = if (e is FingerprintMismatchException) {
                    PairingUi.Mismatch(e.local, e.remote)
                } else {
                    PairingUi.Failed(ErrorText.of(e))
                }
                return@launch
            }
            // The fingerprint shown is the one computed here from the watch's own key.
            state.value = PairingUi.Code(UserCode.display(ticket.start.userCode), ticket.fingerprint)
            state.value = when (val outcome = pairing.await(ticket.start)) {
                is PairingOutcome.Approved -> {
                    gateway.completePairing(outcome.linked)
                    PairingUi.Linked
                }
                PairingOutcome.Expired -> PairingUi.Expired
                PairingOutcome.Denied -> PairingUi.Denied
                is PairingOutcome.Failed -> PairingUi.Failed(ErrorText.of(outcome.error))
            }
        }
    }
}

@Composable
fun PairingScreen(gateway: Gateway, onLinked: () -> Unit, onSettings: () -> Unit) {
    val vm = viewModel { PairingViewModel(gateway) }
    val ui by vm.ui.collectAsStateWithLifecycle()

    LaunchedEffect(ui) { if (ui == PairingUi.Linked) onLinked() }

    val retry: (@Composable androidx.compose.foundation.layout.BoxScope.() -> Unit)? = when (ui) {
        PairingUi.Expired, PairingUi.Denied, is PairingUi.Mismatch, is PairingUi.Failed -> {
            { EdgeButton(onClick = vm::start) { Text("Try again") } }
        }
        else -> null
    }

    ListScreen(edgeButton = retry) { list ->
        when (val state = ui) {
            PairingUi.Preparing -> item { Busy("Making this watch's key…", itemModifier(list)) }
            is PairingUi.Code -> {
                item { ListHeader(modifier = itemModifier(list), transformation = transformation(list)) { Text("Pair this watch") } }
                item { BigCode(state.userCode, itemModifier(list)) }
                item { Note("Type this code in Stormo on your Mac", itemModifier(list)) }
                item {
                    Text(
                        "Key ${state.fingerprint}",
                        modifier = itemModifier(list),
                        textAlign = TextAlign.Center,
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                item { InlineBusy("Waiting for approval", itemModifier(list)) }
            }
            PairingUi.Linked -> item { Busy("Linked", itemModifier(list)) }
            PairingUi.Expired -> item { Note("The code expired before it was typed.", itemModifier(list)) }
            PairingUi.Denied -> item { Note("The pairing was refused on the Mac.", itemModifier(list)) }
            is PairingUi.Mismatch -> {
                item { ListHeader(modifier = itemModifier(list), transformation = transformation(list)) { Text("Warning") } }
                item {
                    Note(
                        "The gateway reported key ${state.remote}, but this watch made ${state.local}. " +
                            "Something between them changed the key. Don't pair through this gateway; check its address.",
                        itemModifier(list),
                        error = true,
                    )
                }
            }
            is PairingUi.Failed -> item { Note(state.message, itemModifier(list), error = true) }
        }
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

/** The pairing code, large: two groups of four on two lines. */
@Composable
private fun BigCode(code: String, modifier: Modifier = Modifier) {
    val groups = code.split('-')
    Column(
        modifier = modifier.fillMaxWidth().semantics { contentDescription = "Code ${UserCode.spoken(code)}" },
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        groups.forEach { group ->
            Text(
                group,
                style = MaterialTheme.typography.displayLarge,
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.Bold,
                color = MaterialTheme.colorScheme.primary,
                maxLines = 1,
            )
        }
    }
}
