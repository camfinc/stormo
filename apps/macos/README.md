# Stormo for macOS

A native SwiftUI app (macOS 26+) for running a Stormo instance without a terminal. It reads the
core's HTTP API and runs every action through a `stormo` binary with `--json`
([docs/api.md](../../docs/api.md)), so it works alongside any installed CLI, and carries its own
`stormo` in `Contents/Helpers` so it also works with none.

```
Stormo.xcodeproj        the app target (folders are synchronized groups; StormoKit is a local package)
Config/                 Stormo.xcconfig (build settings), Identity.xcconfig (bundle id, team)
Stormo/                 the app: scenes and views; AppModel ties StormoKit to them
StormoKit/              all the logic, tested without Xcode's UI (swift test)
scripts/bundle-stormo.sh  build phase: puts stormo in Contents/Helpers and signs it
```

## Build and test

```sh
swift test --package-path apps/macos/StormoKit
xcodebuild -project apps/macos/Stormo.xcodeproj -scheme Stormo build
```

The build phase compiles this checkout's engine (needs `go`) into the app. That copy reports version
`dev` and has no source tree next to it, so it cannot build a sidecar image: for running agents from
a local build, keep your dev CLI installed (`~/.local/bin/stormo`), which the app's Auto choice
prefers. Release builds will bundle the tagged, universal binary instead (`STORMO_BINARY`).

## How it decides things

- **Which instance:** opened with File › Open Instance… (validated by `stormo instance --json`); one
  is active at a time, since one core runs per machine.
- **Which binary:** per instance, Auto picks the binary behind the instance's running core, else an
  installed CLI (PATH, `~/.local/bin`, Homebrew, the instance's own `stormo/bin`), else the one inside
  the app, and pins it. Settings › Command Line changes it.
- **Which environment:** the login shell's (`$SHELL -ilc`), so docker and aws resolve as in a
  terminal; never `SWARM_TARGET`, and every command gets `--target local`. Remote (ECS) work is not
  in this build.
- **The core:** started and stopped only with `stormo core up` / `core down`, so the CLI and the app
  see the same core; quitting the app leaves it running.

## Visual check without Screen Recording

Debug builds take a picture of their own windows and quit:

```sh
STORMO_SNAPSHOT=/tmp/snap .../Stormo.app/Contents/MacOS/Stormo   # writes /tmp/snap/<n>-<title>.png
```

`go run ./examples/office-demo -port 18699` with `SWARM_CORE_PORT=18699` gives it a fleet to show.
The capture does not draw system materials (the sidebar and inspector backgrounds come out blank).

The plan for the rest (the native office, every CLI command, configuration, signing and updates)
is docs/macos-app.md.
