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

The build phase compiles this checkout's engine (needs `go`) into the app, stamped with the
checkout's absolute path (`-ldflags -X …/pkg/version.BuiltFrom=…`). So the bundled copy of a local
build reports the checkout's git revision (with `-dirty` while it has changes) and builds sidecar
images from that checkout, like a CLI built there; the checkout must stay where it was built (or set
`STORMO_SRC`). Release builds bundle the tagged, universal binary instead (`STORMO_BINARY`), which
pulls its published sidecar image.

## How it decides things

- **Which instance:** opened with File › Open Instance… (validated by `stormo instance --json`); one
  is active at a time, since one core runs per machine.
- **Which binary:** per instance, Auto uses the `stormo` inside the app (`Contents/Helpers`), which
  updates with the app; only when that one cannot run, the binary behind the instance's running
  core, else an installed CLI (PATH, `~/.local/bin`, Homebrew, the instance's own `stormo/bin`).
  Auto's pin is re-checked at every launch and every Look again, so an app update moves it. A
  binary picked in Settings › Command Line stays pinned until Back to Auto. A core started by
  another binary keeps running until you restart it (the Core view says so).
- **The terminal's `stormo`:** `~/.local/bin/stormo` (that name only; `swarm` and the rest are never
  touched). At launch the app links it to its own copy when nothing is there (and says so once,
  with a hint when `~/.local/bin` is not on the shell's PATH), and moves a link into any
  `*.app/Contents/Helpers/stormo` (an older Stormo app) to this one, so the command updates with
  the app. Anything else there (a link to a dev checkout, a file, Homebrew's) is yours: Settings ›
  Command Line shows it next to the app's copy and offers Use Stormo's copy in the terminal, which
  replaces it after you confirm. A translocated app or one run from a disk image links nothing.
- **Which environment:** the login shell's (`$SHELL -ilc`), so docker and aws resolve as in a
  terminal; never `SWARM_TARGET`, and every command gets `--target local`. Remote (ECS) work is not
  in this build.
- **The open conversation:** re-read (`stormo conversations <agent> <id>`) while the Chat window
  can be seen and the agent runs, so a follow-up the agent set itself appears without reopening:
  5 s after it last changed, backing off to 30 s while nothing does, sooner when the fleet shows the
  agent doing something; never while a turn of the app's is in flight (that one reads its own
  progress). Unchanged rows are not drawn again.
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
