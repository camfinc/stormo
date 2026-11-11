package stormo.wear.ui

import android.net.Uri
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.navigation.NavHostController
import androidx.wear.compose.material3.AppScaffold
import androidx.wear.compose.material3.MaterialTheme
import androidx.wear.compose.navigation.SwipeDismissableNavHost
import androidx.wear.compose.navigation.composable
import androidx.wear.compose.navigation.rememberSwipeDismissableNavController
import stormo.wear.data.Gateway
import stormo.wear.data.LinkState

object Routes {
    const val SETUP = "setup"
    const val PAIRING = "pairing"
    const val HOME = "home"
    const val REVIEW = "review"
    const val SETTINGS = "settings"
    const val AGENT = "agent/{agent}"
    const val CONVERSATION = "conversation/{agent}/{conversation}"

    fun agent(id: String) = "agent/${Uri.encode(id)}"
    fun conversation(agent: String, id: String) = "conversation/${Uri.encode(agent)}/${Uri.encode(id)}"

    /** Screens that need a link: leaving the link sends the watch back to the start. */
    val LINKED = setOf(HOME, REVIEW, AGENT, CONVERSATION, SETTINGS)

    fun start(link: LinkState) = when (link) {
        LinkState.NeedsGateway -> SETUP
        LinkState.NeedsPairing -> PAIRING
        is LinkState.Linked -> HOME
    }
}

@Composable
fun WearApp(gateway: Gateway) {
    val nav = rememberSwipeDismissableNavController()
    val link by gateway.link.collectAsStateWithLifecycle()
    val start = remember { Routes.start(gateway.link.value) }

    MaterialTheme {
        AppScaffold {
            SwipeDismissableNavHost(navController = nav, startDestination = start) {
                composable(Routes.SETUP) {
                    SetupScreen(gateway, onDone = { nav.restart(Routes.PAIRING) })
                }
                composable(Routes.PAIRING) {
                    PairingScreen(
                        gateway,
                        onLinked = { nav.restart(Routes.HOME) },
                        onSettings = { nav.navigate(Routes.SETTINGS) },
                    )
                }
                composable(Routes.HOME) {
                    FleetScreen(
                        gateway,
                        onAgent = { nav.navigate(Routes.agent(it)) },
                        onReview = { nav.navigate(Routes.REVIEW) },
                        onSettings = { nav.navigate(Routes.SETTINGS) },
                    )
                }
                composable(Routes.AGENT) { entry ->
                    val agent = entry.arguments?.getString("agent").orEmpty()
                    AgentScreen(gateway, agent, onConversation = { nav.navigate(Routes.conversation(agent, it)) })
                }
                composable(Routes.CONVERSATION) { entry ->
                    ConversationScreen(
                        gateway,
                        agent = entry.arguments?.getString("agent").orEmpty(),
                        conversation = entry.arguments?.getString("conversation").orEmpty(),
                    )
                }
                composable(Routes.REVIEW) { ReviewScreen(gateway) }
                composable(Routes.SETTINGS) { SettingsScreen(gateway) }
            }
        }
    }

    // Unlinked here, or revoked on the core while the app was open: back to the start.
    LaunchedEffect(link) {
        if (link is LinkState.Linked) return@LaunchedEffect
        val route = nav.currentDestination?.route ?: return@LaunchedEffect
        if (route in Routes.LINKED) nav.restart(Routes.start(link))
    }
}

/** Navigates to [route] as the only screen on the stack. */
private fun NavHostController.restart(route: String) {
    navigate(route) {
        popUpTo(graph.id) { inclusive = true }
        launchSingleTop = true
    }
}
