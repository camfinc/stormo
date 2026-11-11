# Versions

Every build input of the Wear OS app, pinned. Each is a stable release published at least two weeks
before it was chosen (2026-11-10). Dates are as served: the artifact's `Last-Modified` on Maven
Central or Google's Maven repository, the build time on services.gradle.org (a re-publish can make
one later than the original release; every one is past the two weeks either way). The set was picked to be coherent with what the build machine
already had cached (Gradle 8.14, AGP 8.13.1, Kotlin 2.2.20): AGP 8.13 needs Gradle 8.13 or later and
JDK 17; Kotlin 2.2.20 supports Gradle up to 8.14. Newer releases exist (lint lists them); moving to
one is a deliberate upgrade, checked against the same two-week rule, with this file updated.

## Toolchain

| what | version | released |
|---|---|---|
| Gradle (wrapper, `-all`, SHA-256 pinned) | 8.14 | 2025-04-25 |
| Android Gradle Plugin `com.android.application` | 8.13.1 | 2025-11-10 |
| Kotlin `org.jetbrains.kotlin.android` | 2.2.20 | 2025-09-09 |
| Compose compiler `org.jetbrains.kotlin.plugin.compose` | 2.2.20 | 2025-09-09 |
| JDK | 17 | |
| compileSdk / targetSdk | 36 | |
| minSdk | 30 (Wear OS 3) | |
| build-tools | AGP 8.13's default (35.0.0 and 36.0.0 installed) | |

## Libraries (`gradle/libs.versions.toml`)

| library | version | released | for |
|---|---|---|---|
| `androidx.wear.compose:compose-material3` | 1.5.3 | 2025-10-08 | Material 3 for Wear: scaffold, buttons, cards, dialogs |
| `androidx.wear.compose:compose-foundation` | 1.5.3 | 2025-10-08 | `TransformingLazyColumn` |
| `androidx.wear.compose:compose-navigation` | 1.5.3 | 2025-10-08 | swipe-to-dismiss navigation |
| `androidx.wear:wear-input` | 1.2.0 | 2025-09-10 | the system text input (`RemoteInput`): voice, keyboard |
| `androidx.wear.tiles:tiles` | 1.5.0 | 2025-06-04 | the fleet tile |
| `androidx.wear.protolayout:protolayout` | 1.3.0 | 2025-06-04 | the tile's layout |
| `androidx.wear.protolayout:protolayout-expression` | 1.3.0 | 2025-06-04 | (required by protolayout) |
| `androidx.concurrent:concurrent-futures` | 1.1.0 | 2020-08-19 | the tile's `ListenableFuture` (already a tiles dependency) |
| `androidx.activity:activity-compose` | 1.10.1 | 2025-02-26 | `setContent`, activity results |
| `androidx.lifecycle:lifecycle-viewmodel-compose` | 2.9.4 | 2025-09-17 | view models |
| `androidx.lifecycle:lifecycle-runtime-compose` | 2.9.4 | 2025-09-17 | lifecycle-aware state collection |
| `org.jetbrains.kotlinx:kotlinx-coroutines-android` | 1.10.2 | 2025-04-08 | coroutines (also what the graph resolves to) |

Resolved transitively, not declared: Jetpack Compose runtime/ui 1.9.0 and foundation 1.8.0,
navigation-compose 2.6.0, core 1.15.0, Kotlin stdlib 2.2.20.

No HTTP or JSON library: the app uses `java.net.HttpURLConnection` and the `org.json` Android ships.

## Tests only

| library | version | released | for |
|---|---|---|---|
| `junit:junit` | 4.13.2 | 2021-02-13 | JVM unit tests |
| `org.json:json` | 20250517 | 2025-05-17 | a real `org.json` for the JVM (android.jar has only stubs) |
