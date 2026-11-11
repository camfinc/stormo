package stormo.wear.ui

import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.wear.compose.material3.AlertDialog
import androidx.wear.compose.material3.AlertDialogDefaults
import androidx.wear.compose.material3.Button
import androidx.wear.compose.material3.ButtonDefaults
import androidx.wear.compose.material3.ListHeader
import androidx.wear.compose.material3.MaterialTheme
import androidx.wear.compose.material3.Text
import stormo.wear.BuildConfig
import stormo.wear.core.GatewayUrl
import stormo.wear.data.Gateway
import stormo.wear.data.LinkState

@Composable
fun SettingsScreen(gateway: Gateway) {
    val link by gateway.link.collectAsStateWithLifecycle()
    var url by remember { mutableStateOf(gateway.gatewayUrl) }
    var error by remember { mutableStateOf<String?>(null) }
    var confirmUnlink by rememberSaveable { mutableStateOf(false) }
    val editUrl = rememberTextInput("Gateway address") { input ->
        when (val result = GatewayUrl.normalize(input, gateway.allowLocalCleartext)) {
            is GatewayUrl.Result.Ok -> {
                gateway.setGatewayUrl(result.url)
                url = result.url
                error = null
            }
            is GatewayUrl.Result.Invalid -> error = result.reason
        }
    }

    ListScreen { list ->
        item { ListHeader(modifier = itemModifier(list), transformation = transformation(list)) { Text("Settings") } }
        item {
            Button(
                onClick = editUrl,
                modifier = itemModifier(list),
                transformation = transformation(list),
                label = { Text("Gateway") },
                secondaryLabel = { Text(url ?: "Not set", maxLines = 3) },
            )
        }
        error?.let { e -> item { Note(e, itemModifier(list), error = true) } }
        when (val state = link) {
            is LinkState.Linked -> {
                item {
                    Panel(modifier = itemModifier(list).fillMaxWidth()) {
                        Text("Linked as ${state.name}", style = MaterialTheme.typography.bodyMedium)
                        Text(state.deviceId, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                item {
                    Button(
                        onClick = { confirmUnlink = true },
                        modifier = itemModifier(list),
                        transformation = transformation(list),
                        colors = ButtonDefaults.buttonColors(
                            containerColor = MaterialTheme.colorScheme.errorContainer,
                            contentColor = MaterialTheme.colorScheme.onErrorContainer,
                        ),
                        label = { Text("Unlink this watch") },
                    )
                }
            }
            else -> item { Note("Not linked yet.", itemModifier(list)) }
        }
        item {
            Text(
                "Stormo ${BuildConfig.VERSION_NAME}",
                modifier = itemModifier(list),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                textAlign = androidx.compose.ui.text.style.TextAlign.Center,
            )
        }
    }

    AlertDialog(
        visible = confirmUnlink,
        onDismissRequest = { confirmUnlink = false },
        confirmButton = {
            AlertDialogDefaults.ConfirmButton(onClick = {
                confirmUnlink = false
                gateway.unlink()
            })
        },
        dismissButton = { AlertDialogDefaults.DismissButton(onClick = { confirmUnlink = false }) },
        title = { Text("Unlink this watch?") },
        text = {
            Text("Deletes its key and everything it stored. Revoke it on your Mac too (stormo device revoke).")
        },
    )
}
