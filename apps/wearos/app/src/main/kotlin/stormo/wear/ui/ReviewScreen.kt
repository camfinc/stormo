package stormo.wear.ui

import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.ViewModel
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewModelScope
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.wear.compose.material3.AlertDialog
import androidx.wear.compose.material3.AlertDialogDefaults
import androidx.wear.compose.material3.FilledTonalButton
import androidx.wear.compose.material3.ListHeader
import androidx.wear.compose.material3.MaterialTheme
import androidx.wear.compose.material3.OutlinedButton
import androidx.wear.compose.material3.Text
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import stormo.wear.core.DecideResult
import stormo.wear.core.Decision
import stormo.wear.core.ErrorText
import stormo.wear.core.Review
import stormo.wear.core.ReviewItem
import stormo.wear.data.Gateway

class ReviewViewModel(private val gateway: Gateway) : ViewModel() {
    data class Pending(val item: ReviewItem, val decision: Decision)

    data class State(
        val items: List<ReviewItem>? = null,
        val loading: Boolean = true,
        val confirm: Pending? = null,
        val deciding: Boolean = false,
        val notice: String? = null,
        val error: String? = null,
    )

    private val state = MutableStateFlow(State())
    val ui: StateFlow<State> = state.asStateFlow()

    suspend fun refresh() {
        state.update { it.copy(loading = true) }
        attempt { gateway.review() }
            .onSuccess { items -> state.update { it.copy(items = items, loading = false, error = null) } }
            .onFailure { e -> state.update { it.copy(loading = false, error = ErrorText.of(e)) } }
    }

    /** Asks for confirmation first; app-only items never get that far. */
    fun ask(item: ReviewItem, decision: Decision) {
        if (!Review.decidableOnWatch(item)) return
        state.update { it.copy(confirm = Pending(item, decision), notice = null) }
    }

    fun cancel() = state.update { it.copy(confirm = null) }

    fun confirm() {
        val pending = state.value.confirm ?: return
        state.update { it.copy(confirm = null, deciding = true, error = null) }
        viewModelScope.launch {
            attempt { gateway.decide(pending.item, pending.decision) }
                .onSuccess { result ->
                    state.update { applyResult(it, pending, result) }
                    if (result == DecideResult.Changed) refresh()
                }
                .onFailure { e -> state.update { it.copy(deciding = false, error = ErrorText.of(e)) } }
        }
    }

    private fun applyResult(s: State, pending: Pending, result: DecideResult): State {
        val items = s.items.orEmpty()
        val item = pending.item
        return when (result) {
            DecideResult.Done -> s.copy(
                items = items.filterNot { it.sameAs(item) },
                deciding = false,
                notice = if (pending.decision == Decision.ACCEPT) "Accepted." else "Rejected.",
            )
            DecideResult.Gone -> s.copy(
                items = items.filterNot { it.sameAs(item) },
                deciding = false,
                notice = "Already decided elsewhere.",
            )
            DecideResult.Changed -> s.copy(deciding = false, notice = "This changed; reload.")
            is DecideResult.TryAgain -> s.copy(
                deciding = false,
                notice = result.seconds?.takeIf { it > 0 }?.let { "The review is busy. Try again in $it s." }
                    ?: "The review is busy. Try again shortly.",
            )
            is DecideResult.AppOnly -> s.copy(
                items = items.map { if (it.sameAs(item)) it.copy(appOnly = true, why = result.why ?: it.why) else it },
                deciding = false,
                notice = "This one is decided in the app.",
            )
        }
    }

    private fun ReviewItem.sameAs(other: ReviewItem) = agent == other.agent && kind == other.kind && id == other.id
}

@Composable
fun ReviewScreen(gateway: Gateway) {
    val vm = viewModel { ReviewViewModel(gateway) }
    val ui by vm.ui.collectAsStateWithLifecycle()
    WhileResumed(periodMillis = 60_000) { vm.refresh() }

    ListScreen { list ->
        item { ListHeader(modifier = itemModifier(list), transformation = transformation(list)) { Text("Review") } }
        ui.notice?.let { notice -> item { Note(notice, itemModifier(list)) } }
        if (ui.deciding) item { InlineBusy("Sending…", itemModifier(list)) }
        val items = ui.items
        when {
            items == null && ui.loading -> item { Busy("Reading proposals…", itemModifier(list)) }
            items != null && items.isEmpty() -> item { Note("Nothing to review.", itemModifier(list)) }
            items != null -> items.forEach { review ->
                item { ReviewPanel(review, itemModifier(list)) }
                if (Review.decidableOnWatch(review)) {
                    item {
                        FilledTonalButton(
                            onClick = { vm.ask(review, Decision.ACCEPT) },
                            enabled = !ui.deciding,
                            modifier = itemModifier(list),
                            transformation = transformation(list),
                            label = { Text("Accept") },
                        )
                    }
                    item {
                        OutlinedButton(
                            onClick = { vm.ask(review, Decision.REJECT) },
                            enabled = !ui.deciding,
                            modifier = itemModifier(list),
                            transformation = transformation(list),
                            label = { Text("Reject") },
                        )
                    }
                }
            }
        }
        ui.error?.let { error -> item { Note(error, itemModifier(list), error = true) } }
    }

    val pending = ui.confirm
    AlertDialog(
        visible = pending != null,
        onDismissRequest = vm::cancel,
        confirmButton = { AlertDialogDefaults.ConfirmButton(onClick = vm::confirm) },
        dismissButton = { AlertDialogDefaults.DismissButton(onClick = vm::cancel) },
        title = {
            Text(
                when (pending?.decision) {
                    Decision.REJECT -> "Reject this ${pending.item.kind}?"
                    else -> "Accept this ${pending?.item?.kind ?: "proposal"}?"
                },
            )
        },
        text = { pending?.let { Text("${it.item.agentName}: ${it.item.summary}", maxLines = 4) } },
    )
}

@Composable
private fun ReviewPanel(item: ReviewItem, modifier: Modifier = Modifier) {
    Panel(modifier = modifier.fillMaxWidth()) {
        Text(
            "${item.agentName} · ${item.kind}",
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(item.summary, style = MaterialTheme.typography.bodyMedium)
        Review.verdictLine(item)?.let {
            Text(it, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.primary)
        }
        if (!Review.decidableOnWatch(item)) {
            Text("Decide in the app", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.tertiary)
            item.why?.let { Text(it, style = MaterialTheme.typography.bodySmall) }
        }
    }
}
