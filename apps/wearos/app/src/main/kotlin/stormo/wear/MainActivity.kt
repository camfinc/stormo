package stormo.wear

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import stormo.wear.ui.WearApp

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val gateway = (application as StormoApp).gateway
        setContent { WearApp(gateway) }
    }
}
