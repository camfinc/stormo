package stormo.wear.ui

import android.app.RemoteInput
import android.content.ActivityNotFoundException
import android.view.inputmethod.EditorInfo
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.lifecycle.repeatOnLifecycle
import androidx.wear.compose.foundation.lazy.TransformingLazyColumn
import androidx.wear.compose.foundation.lazy.TransformingLazyColumnItemScope
import androidx.wear.compose.foundation.lazy.TransformingLazyColumnScope
import androidx.wear.compose.foundation.lazy.TransformingLazyColumnState
import androidx.wear.compose.foundation.lazy.rememberTransformingLazyColumnState
import androidx.wear.compose.material3.CircularProgressIndicator
import androidx.wear.compose.material3.MaterialTheme
import androidx.wear.compose.material3.ScreenScaffold
import androidx.wear.compose.material3.SurfaceTransformation
import androidx.wear.compose.material3.Text
import androidx.wear.compose.material3.lazy.TransformationSpec
import androidx.wear.compose.material3.lazy.rememberTransformationSpec
import androidx.wear.compose.material3.lazy.transformedHeight
import androidx.wear.input.RemoteInputIntentHelper
import androidx.wear.input.wearableExtender
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay

/** The scope a screen's list items are written in: the item transformation for edge morphing. */
class ListContext(val spec: TransformationSpec)

/**
 * A scrollable round-screen list with the Material 3 scaffold (time text, scroll indicator) and,
 * when given, an edge button hugging the bottom of the screen.
 */
@Composable
fun ListScreen(
    state: TransformingLazyColumnState = rememberTransformingLazyColumnState(),
    edgeButton: (@Composable BoxScope.() -> Unit)? = null,
    content: TransformingLazyColumnScope.(ListContext) -> Unit,
) {
    val context = ListContext(rememberTransformationSpec())
    if (edgeButton != null) {
        ScreenScaffold(scrollState = state, edgeButton = edgeButton) { padding ->
            TransformingLazyColumn(state = state, contentPadding = padding) { content(context) }
        }
    } else {
        ScreenScaffold(scrollState = state) { padding ->
            TransformingLazyColumn(state = state, contentPadding = padding) { content(context) }
        }
    }
}

/** Width and height handling every list item shares. */
fun TransformingLazyColumnItemScope.itemModifier(list: ListContext): Modifier =
    Modifier.fillMaxWidth().transformedHeight(this, list.spec)

@Composable
fun TransformingLazyColumnItemScope.transformation(list: ListContext): SurfaceTransformation =
    SurfaceTransformation(list.spec)

/** Centered body text for a list. */
@Composable
fun Note(text: String, modifier: Modifier = Modifier, error: Boolean = false) {
    Text(
        text = text,
        modifier = modifier.fillMaxWidth().padding(horizontal = 8.dp),
        textAlign = TextAlign.Center,
        style = MaterialTheme.typography.bodyMedium,
        color = if (error) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
    )
}

/** A spinner with a line under it. */
@Composable
fun Busy(text: String, modifier: Modifier = Modifier) {
    Column(
        modifier = modifier.fillMaxWidth().padding(vertical = 8.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        CircularProgressIndicator(modifier = Modifier.size(32.dp))
        Text(text, style = MaterialTheme.typography.bodySmall, textAlign = TextAlign.Center)
    }
}

/** Inline progress: a small spinner and a line beside it. */
@Composable
fun InlineBusy(text: String, modifier: Modifier = Modifier) {
    Row(
        modifier = modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 4.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.CenterHorizontally),
    ) {
        CircularProgressIndicator(modifier = Modifier.size(20.dp))
        Text(text, style = MaterialTheme.typography.bodySmall)
    }
}

/** A non-interactive rounded panel for content (a message, a review item). */
@Composable
fun Panel(modifier: Modifier = Modifier, highlight: Boolean = false, content: @Composable ColumnScope.() -> Unit) {
    val colors = MaterialTheme.colorScheme
    Column(
        modifier = modifier
            .background(
                color = if (highlight) colors.primaryContainer else colors.surfaceContainer,
                shape = RoundedCornerShape(20.dp),
            )
            .padding(horizontal = 14.dp, vertical = 10.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp),
        content = content,
    )
}

/** A small round dot, colored by an agent's state. */
@Composable
fun StateDot(state: String, modifier: Modifier = Modifier) {
    val colors = MaterialTheme.colorScheme
    val color = when (state) {
        "running" -> colors.primary
        "starting", "restarting" -> colors.tertiary
        "exited" -> colors.error
        else -> colors.outline
    }
    Box(modifier.size(12.dp).background(color, RoundedCornerShape(6.dp)))
}

private const val INPUT_KEY = "stormo_text"

/**
 * The system text input (voice, keyboard, handwriting) as a function that opens it; [onText] gets
 * the trimmed text when the person confirms something non-empty.
 */
@Composable
fun rememberTextInput(label: String, onText: (String) -> Unit): () -> Unit {
    val current by rememberUpdatedState(onText)
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        val data = result.data ?: return@rememberLauncherForActivityResult
        val text = RemoteInput.getResultsFromIntent(data)?.getCharSequence(INPUT_KEY)?.toString()?.trim()
        if (!text.isNullOrEmpty()) current(text)
    }
    return remember(label, launcher) {
        {
            val input = RemoteInput.Builder(INPUT_KEY)
                .setLabel(label)
                .wearableExtender {
                    setEmojisAllowed(true)
                    setInputActionType(EditorInfo.IME_ACTION_DONE)
                }
                .build()
            val intent = RemoteInputIntentHelper.createActionRemoteInputIntent()
            RemoteInputIntentHelper.putRemoteInputsExtra(intent, listOf(input))
            try {
                launcher.launch(intent)
            } catch (_: ActivityNotFoundException) {
                // No input method on this device: nothing to open.
            }
        }
    }
}

/** Runs [action] now and then every [periodMillis] while the screen is resumed. */
@Composable
fun WhileResumed(periodMillis: Long, action: suspend () -> Unit) {
    val owner = LocalLifecycleOwner.current
    val current by rememberUpdatedState(action)
    LaunchedEffect(owner) {
        owner.lifecycle.repeatOnLifecycle(Lifecycle.State.RESUMED) {
            while (true) {
                current()
                delay(periodMillis)
            }
        }
    }
}

/** Runs [block], turning any failure but cancellation into a [Result]. */
suspend fun <T> attempt(block: suspend () -> T): Result<T> = try {
    Result.success(block())
} catch (e: CancellationException) {
    throw e
} catch (e: Exception) {
    Result.failure(e)
}
