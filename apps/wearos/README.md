# Stormo for Wear OS

A standalone Wear OS app (Kotlin, Jetpack Compose for Wear OS, Material 3) that reaches a Stormo
fleet through its **device gateway** ([docs/devices.md](../../docs/devices.md), the contract this
app is built against). It needs no phone app: it works over Wi-Fi, LTE or the phone's Bluetooth
proxy. From the watch you can see the fleet, chat with the agents the watch may talk with, and
accept or reject the proposals the gateway lets a device decide. A tile shows the fleet at a glance.

```
identity.properties      the publisher's application id and the default gateway address (one file)
gradle/libs.versions.toml  every dependency, pinned (dates in VERSIONS.md)
app/src/main/kotlin/stormo/wear/
  core/                  plain Kotlin, tested on the JVM: GatewayClient (HTTP + JSON), DeviceSession,
                         Pairing, Review, Chat, the encodings (base64url, JWK, fingerprint, proof)
  data/                  Android only: the Keystore key, SharedPreferences, the app's Gateway
  ui/                    the screens and their view models
  tile/                  the fleet tile
app/src/debug/res/xml/   the debug build's network config (cleartext to the emulator host only)
app/src/test/            JVM unit tests (a fake transport and a software key)
```

## Build and test

JDK 17 and the Android SDK (platform 36) are needed. Point the build at the SDK with
`ANDROID_HOME` (or an ignored `local.properties` with `sdk.dir=…`):

```sh
cd apps/wearos
export ANDROID_HOME="$HOME/Library/Android/sdk"
./gradlew :app:assembleDebug :app:testDebugUnitTest :app:lintDebug
```

The debug APK is `app/build/outputs/apk/debug/app-debug.apk`. `:app:assembleRelease` builds a
minified, unsigned release APK; sign it with your own key for distribution.

## Install on a watch (Wi-Fi debugging)

1. On the watch: Settings › System › About › tap *Build number* seven times; then Settings ›
   Developer options › turn on *ADB debugging* and *Wireless debugging* (the watch and the computer
   on the same Wi-Fi).
2. *Wireless debugging* › *Pair new device* shows an address and a code:
   `adb pair <watch-ip>:<pairing-port>` and type the code.
3. `adb connect <watch-ip>:<port>` (the port shown on the *Wireless debugging* screen itself).
4. `./gradlew :app:installDebug`, or `adb -s <watch-ip>:<port> install -r app/build/outputs/apk/debug/app-debug.apk`.

On a Wear OS emulator, a gateway on the development machine is `http://10.0.2.2:<port>` (debug
builds only; for example the `publish` port of the `devices.gateway` capability).

## Set the gateway address

The first screen asks for the gateway's base URL: the HTTPS hostname your tunnel or proxy serves
the gateway on (docs/devices.md, *Setting it up*), for example `https://fleet.example.com`. Tap
*Gateway* and say or type it (`https://` is added when left out). *Continue* checks
`GET /device/v1/health` before saving it. A build can start with an address already filled in:
set `gatewayUrl=` in `identity.properties` (empty in this repository). The address can be changed
later in Settings.

Release builds accept only `https://`. Debug builds also accept `http://` to `10.0.2.2`,
`localhost` and `127.0.0.1`, matching their network security config.

## Pair

1. The watch makes its key and shows a code (two groups of four letters) and the key's
   fingerprint, with "Type this code in Stormo on your Mac".
2. On the Mac, type the code in the Stormo app's Linked devices page, or with the CLI:
   `stormo device link WDJB-MJHT --grant status,review --agents support`. Check that the
   fingerprint it shows is the one on the watch before approving.
3. The watch polls until the owner approves (or refuses, or the code expires: *Try again* starts
   over with a new key) and opens the fleet.

What the watch shows follows its grants (`GET /device/v1/me`): the fleet with `status`, the Review
button with `review`, and chat only with the agents `chat` covers. Grants are changed on the Mac
(`stormo device grants`).

## Screens

- **Fleet** (home): each agent's state, the tool it runs now, unread messages, approvals waiting and
  proposals to review; refreshed every 30 s while shown. Tap an agent to chat with it.
- **Chat**: the agent's recent conversations, *New* to start one, and a conversation's messages with
  *Reply* (voice or keyboard). The watch follows the reply turn every 3 s; one turn runs at a time.
- **Review**: each proposal with its summary and the AI's verdict. Items the gateway allows on a
  device have *Accept* and *Reject*, each confirmed first; the decision carries the item's
  `version`, so a proposal that changed meanwhile is refused ("This changed; reload") rather than
  decided blind. Skills, schedules and anything else marked `appOnly` say *Decide in the app* and why.
- **Settings**: the gateway address, the link, and *Unlink this watch*.
- **Tile**: agents running, approvals waiting and proposals to review, from the last read (refreshed
  when older than 5 minutes); tap it to open the app.

## Security

- **The key never leaves the watch.** It is an Android Keystore key (alias `stormo-device`, EC P-256,
  sign only, SHA-256), not exportable, usable only while the watch is unlocked, in StrongBox when the
  watch has one (else the TEE), with a hardware attestation chain sent at pairing when the watch
  gives one. The gateway gets only the public key, as a JWK.
- **The session token is kept in memory only**, never on disk; a new one is made by signing a fresh
  challenge (`stormo-device-session`, the device id and the nonce) whenever it is needed. The pairing
  device code also stays in memory.
- **The fingerprint is checked**: the watch computes its key's fingerprint itself and shows that one;
  if the gateway reports a different one, the pairing stops with a warning and no code is shown.
- **HTTPS only** in release builds (network security config, system CAs only), no redirects
  followed, no caching, 10 s connect and 30 s read timeouts.
- Nothing is logged: no tokens, codes or messages.
- On disk: the gateway address, the device id and name, and the tile's counts. Excluded from
  backups and device transfer.
- *Unlink this watch* deletes the key and everything stored. It cannot revoke the link on the core
  (a device can't); revoke it on the Mac too: `stormo device revoke <device>`. When the core answers
  `device_unknown` (revoked, expired), the watch deletes its key and returns to pairing.

## Identity

`identity.properties` holds the publisher's application id (`applicationId=`) and the default
gateway address. Everything else (the Kotlin package and the namespace, `stormo.wear`) is neutral; a
fork changes only that file.

## Not done yet

- Verified with the JVM unit tests, lint and builds only, not yet on a watch or an emulator.
- Attestation is sent but, like the gateway, not verified by anything yet.
