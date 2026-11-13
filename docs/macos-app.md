# Stormo for macOS

Status: **built and released**: every release attaches `Stormo-<version>.dmg`, signed and
notarized. This page was written as the plan, so "today" below means before the app. Since then:
the app connects to remote cores by URL and token (docs/remote-core.md), the core's API has
tokens and actions (docs/api.md), and the app gained the pages the README lists. Not built:
Sparkle updates (a new version is a new DMG). How the app works now: apps/macos/README.md.

A native macOS app for running a Stormo instance on the owner's Mac: every local `stormo` command,
the instance's configuration, the office, the fleet and the core, without a terminal. It ships
with the engine (this repo, `apps/macos/`), carries its own `stormo` binary so it works with no
CLI installed, and works alongside any `stormo` CLI already on the machine: whatever the app does,
the CLI sees, and the other way round.

Not a web wrapper. The app is SwiftUI throughout, the office included; it reads the core's JSON
API and runs `stormo` commands. The web office (`pkg/core/ui/`) stays as it is for browsers.

**Scope of this build: local only.** Anything that touches AWS (`--remote`/`-r`, `--target remote`,
`handoff`, `deploy render*`, `secrets push`, S3 nap stores) comes later with cloud instances
(*Cloud, later* at the end).

## Decided

| | decision |
|---|---|
| UI | SwiftUI, **macOS 26 minimum** (current APIs, the system's current look, no back-deployment). No web view |
| actions | every action is a `stormo` subprocess with `--json` output; the core's HTTP API is read-only |
| config | edited in the app, written only through `stormo` (new `config` and `secrets set` commands), validated by the engine before anything is replaced |
| cores | one active instance, one core per machine (the port is machine-wide) |
| remote | out of scope for this build |
| identity | `com.camfsolutions.stormo`, "Stormo" (the publisher's bundle id); set in one xcconfig (`Config/Identity.xcconfig`) so a fork can change it |
| distribution | Developer ID, hardened runtime, notarized, no App Sandbox; signing and notarization run in CI from credentials made on developer.apple.com and App Store Connect |
| updates | Sparkle 2, appcast and DMG on the public `camfinc/stormo` GitHub Releases |
| versions | app version = engine tag; the bundled `stormo` is that release |

### Why actions go through the CLI and not the core

`docs/core.md` §3 plans actions inside the core for the web office. The app needs every CLI command
plus configuration, and the CLI is the only place all of them exist:
- Many commands need no core at all (`check`, `build`, `inspect`, `bench`, `secrets`, `learn`,
  `skill`, `slack manifest`); through the core, the app could not run them while it is down.
- Re-implementing ~35 commands as HTTP routes would duplicate the CLI and need an owner key on
  the core first (a browser page can POST to `127.0.0.1`; today the API has no auth).
- "Works with any local CLI" then holds by construction: the app is one more caller of it.
- The guards stay in one place (the one-place rule, pinned names, action isolation, the ledger's
  PII refusals).

If the core later grows HTTP actions for the web office, the app can switch per action.

## What exists today (the ground the app stands on)

| | today | source |
|---|---|---|
| reads | `GET /health`, `/api/fleet` (agents, units, office timeline, robot, gateway, server `now`), `/api/gateway` (adds the account email and model list), `/avatars/<id>.png`. Loopback only, no key. No SSE: the web page polls `/api/fleet` every 2.5 s | `pkg/core/server.go` |
| instance look | clocks and `office.units` reach the page only as `window.STORMO` injected into `index.html`; no JSON route | `server.go` |
| actions | none over HTTP. Every action is a CLI command; the web inspector copies `swarm …` strings | `cmd/stormo/main.go`, `pkg/ops` |
| machine output | only `inspect` (JSON) and the core API. No `--json` anywhere; `stormo version` prints one string | `main.go` |
| config writes | none: instance files are edited by hand; `secrets init` and `share` rewrite `secrets.local.yaml` | `pkg/secrets` |
| core lifecycle | `core up` re-execs `os.Executable() core serve` (setsid, cwd and `STORMO_INSTANCE` = instance root), pid and log in `<instance>/.swarm/core/`, waits for `/health`. `core down` signals the pid-file process only. Port `SWARM_CORE_PORT` (18600), machine-wide | `pkg/core/cli.go` |
| login | `core login` runs in the CLI process: loopback callback on 1455 (or an OS port), opens the browser with `open`, 10 min timeout; the core picks up `auth/chatgpt.json` by mtime | `pkg/core/llm/chatgptauth.go` |
| version | `-X …/pkg/version.Version=<tag>` on releases; dev builds report the source checkout's git rev. The version picks the sidecar image: released → pull `ghcr.io/camfinc/stormo-sidecar:<version>`, dev → build `stormo-sidecar:<rev>` from source. No API version | `pkg/version`, `pkg/local/sidecar.go` |
| releases | none published yet. The workflow builds `stormo_<tag>_darwin_{arm64,amd64}.tar.gz` + `SHA256SUMS` (`CGO_ENABLED=0`, unsigned, no universal binary) and pushes the sidecar image | `scripts/release-build.sh`, `.github/workflows/release.yml` |
| external tools | `docker` + compose v2 (local lifecycle, status, logs, nap-now, review, bench, slack manifest, sidecar image), `git`, `open`, and `aws` read-only for the one-place check on a local start. The core itself shells out to docker for status | `pkg/local`, `pkg/place` |
| the office | 1,199 lines of JS + 862 of CSS: DOM and CSS-drawn furniture, inline-SVG people, all geometry in the browser (CSS grid, `getBoundingClientRect`). The server sends only the timeline (`activity`, `phase`, `start`, `end`, `slot`, `seq`) | `pkg/core/ui/`, `pkg/core/office.go` |

## Command coverage

Every local command has a place in the app. `--json` is what the app needs from it (Phase 0).

| command | in the app | `--json` |
|---|---|---|
| `stormo` / `status` / `ps` | Agents (from the core; from `status --json` while the core is down) | yes |
| `start`, `stop`, `restart` (+ `--keep-home`, `--nap-interval`, `--allow-prod-token`, `--rebuild-sidecar`) | agent and fleet actions; "Start with options…" sheet | yes |
| `logs <a> [-f]` | Logs window per agent | no (a text stream) |
| `chat <a> "msg" [--session s]` | Chat panel in the inspector, one turn per send | yes |
| `nap-now <a>` | agent action | yes |
| `learn [agent…]` | Learning: "Collect now" | yes |
| `learn list`, `accept`, `reject`, `pin`, `unpin`, `edit --text`, `promote --scope` | Learning: proposals per agent, review actions, edit sheet, promote to unit/group | yes |
| `learn skills`, `skill-accept`, `skill-reject` | Learning: skill proposals with their files | yes |
| `dream <a>` (local store) | Learning: "Fold naps" | yes |
| `check [agent…]` | Configure: validation after every save; "Check all" | yes |
| `build <a>` | agent action "Build baseline", Show in Finder | yes |
| `inspect <a>` | inspector tab "Compiled" (JSON tree) | already JSON |
| `bench <a> [--runner mock\|docker] [--env-file] [--tag]` | Bench: scenarios per agent, run, results | yes |
| `review [agent…] [--since]` | Review: run the sweep, read the report | yes (+ the report file) |
| `shared [--dir]` | Configure › Instance: "Create local shared space" | yes |
| `slack manifest <a> [--dev]` | Configure › agent › Channels: generate, copy, save | yes |
| `secrets init`, `share`, `check`, `env` | Configure › Secrets | yes |
| `core up`, `down`, `status`, `login`, `logout` | Core and ChatGPT plan sections, toolbar, menu bar | yes |
| `skill install`, `uninstall`, `show` | Settings › Agent skill (Claude Code, Codex; user or project scope) | yes |
| `version` | About, Settings › Command line | yes |
| `help` | Help menu: the command reference | no |

Not in the app: `core serve` (the app always uses `core up`), `rehydrate` and `nap` (run inside the
sidecar), and the remote-only ones listed under *Cloud, later*.

**Run a command…** (⇧⌘P): a palette that runs any local `stormo` command with arguments and
streams its output into a console sheet. It covers commands before their dedicated UI exists and any
the engine adds later. It refuses `-r`, `--remote`, `--target remote`, `handoff`, `deploy` and
`secrets push` in this build, and adds `--target local` itself (the environment carries no
`SWARM_TARGET`).

## Configuration management

The app edits the instance's files, through the engine:

| what | file | editor |
|---|---|---|
| instance | `stormo.yaml`: `name`, `org`, `author`, `office.clocks`, `office.units` (hue, floor, wall, desk defaults), `scrub` (token prefixes, rules) | form; office looks with a live preview |
| bridge actions | `bridge/actions.yaml` | list + form, raw YAML |
| units | `units/<unit>/unit.yaml`, `knowledge/*.md` | form; Markdown editor |
| agents | `agents/<id>/agent.yaml`: unit, role, persona, engine kind and `image_tag`, model, provider, `local.via`, channels, secret and optional secret **names**, env, actions, state, learning | form |
| behaviour | `agents/<id>/SOUL.md` | Markdown editor |
| engine files | `agents/<id>/hermes/config.base.yaml`, `cron.jobs.json` | raw YAML; cron as a table |
| bench | `agents/<id>/bench/*.yaml` | raw YAML, with Run |
| skills, plugins, scripts | `agents/<id>/{skills,plugins,scripts}/` | file list; open in the default editor |
| personas | `personas/<slug>/persona.md` frontmatter `desk:` and `sprite:` | form with a live preview of the desk and the person (the office's own drawing code) |
| secrets | `secrets.local.yaml` values | matrix agent × name (set, missing, shared); secure fields |
| new | an agent, a unit | sheets (`stormo new agent|unit`, Phase 0) |

Rules:
- **The engine is the only writer of YAML.** Forms are built from `stormo config schema --json`;
  saving sends a patch to `stormo config apply`, which edits the YAML node tree (comments and order
  kept), validates the result with the same code as `check`, and replaces the file atomically or
  refuses with the errors. The app shows the errors on the fields. Raw YAML goes through
  `stormo config write` with the same validation.
- **Markdown and scripts** (`SOUL.md`, knowledge, skills) are plain text with nothing for the engine
  to validate: the app writes them directly (atomically), then runs `check <agent> --json`.
- **Conflicts:** every write checks the hash of the version the editor loaded: the engine does it
  for YAML (`--if-hash`), the app itself for the files it writes directly. A file changed on disk
  since (an editor, a `git pull`) is refused and the editor reloads. The app watches
  the instance with FSEvents and refreshes open editors.
- **Secret values never pass through argv or YAML forms**: `stormo secrets set <agent|shared> NAME`
  reads the value from stdin; the app never reads `secrets.local.yaml` itself and never shows a
  stored value (only set / missing / shared). Manifests keep names only.
- **Read-only here**: `names:` (pinned: they name deployed infrastructure and agents' skills) and
  `deploy.*` (cloud, later), with the reason shown. **Never editable**: `learnings/` (the ledger
  changes only through `learn`, which keeps the PII scrub and promotion refusals), `.swarm/`,
  `dist/`, avatar bakes (paid, a human step).
- **Git**: the app shows which instance files changed (`git status`, a diff view) and never commits,
  pulls or switches branches; the owner does that.

## Repository layout

```
apps/macos/
  Stormo.xcodeproj           app target only; folders as synchronized groups (little pbxproj churn), no generator
  Config/Identity.xcconfig   PRODUCT_BUNDLE_IDENTIFIER, DEVELOPMENT_TEAM, MACOSX_DEPLOYMENT_TARGET = 26.0
  Stormo/                    the app: App.swift, scenes, views, commands, Assets.xcassets, AppIcon.icon,
                             Info.plist, Stormo.entitlements (hardened runtime, no sandbox)
  StormoKit/                 local Swift package, all logic, `swift test`-able without Xcode UI:
    Sources/StormoKit/
      API/                   Codable models for /health, /api/fleet, /api/gateway, /api/instance; CoreClient (polling)
      CLI/                   BinaryResolver, Runner (Process + NDJSON stream), typed commands, the palette's parser
      Config/                schema-driven forms model, patches, hashes, FSEvents watcher
      Environment/           shell env capture, tool resolution (docker, git, aws)
      Instance/              instance list, validation
      Core/                  CoreSupervisor (attach, up, down, health, compat)
      Fleet/                 FleetStore (@Observable), snapshot diffing → events, notification source
      Office/                FloorLayout, WalkGraph, Playback, sprite and furniture drawing
    Tests/StormoKitTests/    golden fixtures from the engine, layout, playback, resolver, config patches
  scripts/
    build.sh                 universal stormo + xcodebuild archive + sign (inside out)
    notarize.sh              notarytool submit, staple, DMG (hdiutil)
  README.md
```

Rules added to `stormo/CLAUDE.md` when the app lands: no organisation in `apps/` beyond
`Config/Identity.xcconfig`; Swift checks are `swift test --package-path apps/macos/StormoKit`,
`xcodebuild -project apps/macos/Stormo.xcodeproj -scheme Stormo build` and `swift format lint` (in
the toolchain, no new dependency). Swift dependencies: Sparkle only, pinned in `Package.resolved`, a
release at least two weeks old (the engine's rule).

## Phase 0: the engine contract (Go)

Additive changes so a second client can rely on the engine instead of reading `app.js` or parsing
help text. No persisted format changes.

1. **Core identity**: `/health` grows `version` and `api` only (it also reaches agent containers
   through the bridge listeners, so no host paths); a new loopback-only `GET /api/core` gives
   `version`, `api`, `pid`, `exe` (symlinks resolved), `port`, `startedAt` and `instance` (`root`,
   `name`, `org`, `slug`). **Built** (docs/api.md).
2. **`GET /api/instance`**: `name`, `org`, `slug`, `clocks`, `office.units`: the data behind
   `window.STORMO`. The web page keeps its injection. **Built.**
3. **API version**: `version.API`, an integer in `pkg/version`, bumped on a breaking change to the
   HTTP API, the `--json` events or the config schema; additive fields do not bump it.
4. **`--json` everywhere the coverage table says so**, through one output layer in `cmd/stormo`
   (text by default, NDJSON with `--json`), converted command by command:
   `{"event":"step","msg"}`, `{"event":"auth_url","url"}` (login), and last either
   `{"event":"result","data":{…}}` or `{"event":"error","msg","code"}`; exit 0/1/2. **Built**: the
   layer, `version` and `instance`; other commands gain `result` data one by one. Also `stormo version --json`
   (`version`, `api`, `released`, `exe`) and `stormo instance --json` (the loaded instance or the load
   error: the app's cheap "is this folder an instance" check).
5. **Configuration commands** (new, local):
   - `config schema --json`: a JSON Schema per file kind (stormo.yaml, unit.yaml, agent.yaml,
     bridge actions, persona frontmatter, cron jobs), hand-written in the engine (no schema library:
     the dependency rule) and kept in step with the Go types and their validation by a test (enums,
     patterns, pinned and read-only fields marked).
   - `config show <file> --json`: the parsed document and its hash.
   - `config apply <file> --if-hash <h>`: a JSON merge patch on stdin, applied to the yaml.v3 node
     tree (comments and order kept; for `persona.md`, to its YAML frontmatter, leaving the Markdown
     body as it is), validated like `check`, written atomically; refuses `names.*` and `deploy.*`
     in this build.
   - `config write <file> --if-hash <h>`: the whole file on stdin in its own format (YAML, or JSON
     for `cron.jobs.json`), same validation.
   - `new agent <id> --unit <u> [--persona <slug>]`, `new unit <id>`: scaffolds that pass `check`.
   - `new instance <dir> --name <n> --slug <s>`: a new instance from `examples/minimal`, which the
     binary embeds (`go:embed`), so the bundled binary can do it on a Mac with no engine checkout.
   - `secrets set <agent|shared> NAME` (value on stdin), `secrets unset`.
6. **`core login --no-open`**: emits `auth_url` and leaves opening it to the caller.
7. **Golden fixtures**: a Go test runs the core against `examples/minimal` with a fake fleet (the
   office-demo's fake engine), a fixed clock and a fixed seed, and writes
   `testdata/api/{health,fleet,gateway,instance}.json`, `testdata/cli/*.ndjson` and
   `testdata/config/schema.json` (`-update` regenerates). StormoKit's tests decode the same files.
8. **`docs/api.md`**: the HTTP routes, the `--json` events, the config commands, the `api` rule.

Done when: `go test ./...` covers the new fields, events and config commands (comments survive an
apply; an invalid patch leaves the file untouched; a stale `--if-hash` is refused; `names.*` is
refused; the schema test fails when a manifest field is added without it); `new instance` from a
release build, outside any checkout, produces an instance that passes `check`; regenerating the
fixtures twice gives no diff.

## Phase 1: app skeleton, binary, environment, core

### Binary resolution

Candidates, each probed with `version --json` (a binary without it is shown as "too old"):

| candidate | where |
|---|---|
| bundled | `Stormo.app/Contents/Helpers/stormo`, always present, same version as the app |
| on PATH | `command -v stormo` in the captured shell PATH |
| usual places | `~/.local/bin`, `/opt/homebrew/bin`, `/usr/local/bin` |
| vendored | `<instance>/stormo/bin/stormo` (an instance that vendors the engine as a submodule) |
| custom | a path chosen in Settings |

Candidates that resolve to the same file (a `~/.local/bin` symlink into a vendored build) are shown
once.

Per instance, the setting is **Auto** (the default) or a fixed choice. Auto resolves once, when the
instance is added, and is then pinned:
1. a core already running for this instance → the binary that started it (`/api/core` `exe`), if its
   `api` is supported;
2. else the installed CLI (PATH, usual places, vendored, in that order) if its `api` is supported;
3. else the bundled binary.

The pinned binary runs every command and every `core up` for that instance, since it decides the
core's code and the sidecar image agents get on their next start. Changing it is a user action with
a warning: "Agents started from now on use sidecar `<version>`." An app update changes the bundled
binary under the same path, so the same warning shows after an update when the bundled binary is
pinned. A dev binary (version is a git rev) is fine: it builds its sidecar from its source tree.

**Install command line tool** (Settings and first run, only when no `stormo` is on PATH): a symlink
`~/.local/bin/stormo → Stormo.app/Contents/Helpers/stormo`, with a note if `~/.local/bin` is not on
PATH. Sparkle replaces the bundle in place, so the symlink follows app updates. Never overwrites an
existing `stormo`.

### Environment

A Finder-launched app gets launchd's PATH (`/usr/bin:/bin:/usr/sbin:/sbin`), so `docker` would not
resolve, for the app's commands or for a core it starts (the core runs docker for status). On launch
and on demand, the app captures the environment a terminal would see: `$SHELL -ilc` (login and
interactive, so both `.zprofile` and `.zshrc` count) printing `env -0` between markers, stdin from
`/dev/null`, a 5 s timeout. It keeps `PATH`, `HOME`, `DOCKER_HOST`, `DOCKER_CONTEXT`,
`SSH_AUTH_SOCK`, `AWS_PROFILE`, `AWS_REGION`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`,
`SWARM_CORE_PORT` and `SWARM_SECRETS_FILE`, and **drops `SWARM_TARGET`** (a `SWARM_TARGET=remote`
in the shell would otherwise turn every command remote). Fallback PATH entries:
`/opt/homebrew/bin`, `/usr/local/bin`, `~/.orbstack/bin`. Settings shows what resolved and allows a
path override per tool. Every spawn gets that environment plus `STORMO_INSTANCE=<root>`, cwd = the
instance root, an explicit `--instance <root>` and, on every command that takes it,
`--target local`.

`aws` stays a resolved tool for one read-only use: a local start of an agent that shares its Slack
app with ECS first checks that it is not running there (`GuardStart`, `pkg/place`). Without `aws`
or credentials that side is unknown and the CLI refuses the start; the app shows why and offers the
two ways out the CLI names: a dev Slack app for local runs (Configure › Secrets) or "Start anyway"
(`--allow-prod-token`, behind a confirmation saying both places would share one Slack app).

Docker check: `docker info` through that environment. Not running → a banner with **Open OrbStack**
or **Open Docker Desktop** (whichever is installed, via `NSWorkspace`).

### Instances

- Added with File › Open Instance… (`NSOpenPanel`, validated with `<bin> instance --json`) or
  picked up from `STORMO_INSTANCE` in the captured environment.
- Stored as paths in `UserDefaults`; the toolbar switcher lists them by `name`.
- One active at a time. Switching while a core for the old instance runs → "Stop the core for
  *A* and start *B*?" (`core down` with A's binary, `core up` with B's).

### Core supervisor

- **Attach:** poll `/health` (1 s while starting, then with the fleet poll) and read `/api/core`
  once per core (its `startedAt` changes on a restart). Running for another instance → banner with Stop. Running with an unsupported `api` → banner naming its version and
  binary, with **Restart with <pinned binary>**. Running an older build of the pinned binary
  (typically after an app update) → **Restart the core to update**.
- **Start / stop:** `<bin> core up --json` / `core down --json`; progress in the toolbar; on failure,
  the tail of `.swarm/core/core.log`. A core started by something other than `core up` (no pid
  file) is reported, not killed, as the CLI does. Quitting the app leaves the core running (a
  setting stops it on quit).
- **Login:** `<bin> core login --json --no-open`: a sheet "Continue in your browser" opens the URL
  with `NSWorkspace` (and offers Copy); cancel sends SIGTERM; the sheet closes when `/api/gateway`
  reports `login: ok`. Logout the same way.
- **Polling:** `/api/fleet` every 2.5 s while a window is visible (as the web), every 10 s with only
  the menu bar, paused on sleep and screen lock, backoff while the core is down. `URLSession`
  ephemeral configuration: nothing cached to disk. Clock offset from `now`, as the web.

### The shell

- **Main window**: `NavigationSplitView`. Sidebar: Office · Agents · Learning · Bench · Review ·
  ChatGPT plan · Core, and **Configure** (Instance, Units, Agents, Personas, Bridge, Secrets).
  Toolbar: instance switcher, core status, Start/Stop core, changed-files badge. `.inspector` on
  the right for the selected agent from any section.
- **Menu bar** (`MenuBarExtra`, window style): instance and core status; On shift / Working /
  Attention counts; one row per agent (state dot, "Working · Slack", "Idle 12m"); plan status; Open
  Stormo, Settings, Quit. Template icon from the brand mark, with an attention variant.
- **Dock**: shown while a window is open; menu-bar-only when all windows close (setting).
- **Launch at login** via `SMAppService.mainApp`; launched at login, the app opens menu-bar-only and,
  if "Start the core automatically" is on, runs `core up`.
- **Settings**: General (login item, Dock, menu bar), Instances, Command line (binary per instance,
  candidates with version and `api`, Install command line tool), Core (auto-start, stop on quit),
  Tools (resolved docker, git and aws, overrides, environment preview), Notifications, Agent skill.
- **First run**: pick an instance (or create one, Phase 3) → environment and Docker check → binary
  (Auto explained) → start the core → sign in to ChatGPT (skippable).

Done when: a Finder-launched app, on a Mac with no `stormo` on PATH, opens an instance, starts the
core with the bundled binary, shows the fleet in the menu bar, and `stormo core status` in a terminal
sees the same core; and with the dev CLI installed, Auto picks it and attaches to a core the CLI
started.

## Phase 2: operate (every local command)

- **Agents** (`Table`): portrait, name, unit, state, activity label, model, last nap, pending
  learnings and skills, LLM calls today. Sort, filter by unit and state, multi-select. Context menu
  and the Agent menu: Start, Start with options…, Stop, Restart, Nap now, Collect learnings, Build
  baseline, Logs, Chat, Show in Finder, Copy command.
- **Inspector**: portrait, role, unit, state and health detail, platforms (needs-attention), running
  jobs, newest session source and time, model, usage today, last nap and interval, the robot's last
  note, pending learnings; tabs Overview · Chat · Compiled (`inspect`).
- **Actions** run the pinned binary with `--json`; progress in the inspector and a toolbar activity
  indicator; errors as alerts with the CLI's message. Destructive local ones (stop all, reject a
  learning, uninstall the skill) ask first in a native alert.
- **Logs**: a window per agent streaming `logs -f` (monospaced, search, pause/follow, copy); closing
  it ends the process.
- **Chat**: one turn per send through `chat --json`, session name editable, transcript kept for the
  window's life only.
- **Learning**: per agent, proposals by status with accept, reject, pin, unpin, edit (sheet; notes
  that editing clears the PII flag), promote to unit or group; skill proposals with their files and
  accept/reject; "Collect now" (`learn`) and "Fold naps" (`dream`). Accepting stays a human decision;
  the app is that human's button.
- **Bench**: scenarios per agent, runner mock or docker, env file, tag; results per scenario.
- **Review**: run the sweep (`--since`), read the Markdown report rendered natively, open the raw
  JSON. Reports stay in `.swarm/review/` (they can hold PII).
- **ChatGPT plan**: sign-in state, account, "Using ChatGPT plan", plan limit and when it lifts,
  **Manage usage**, in flight / queued / concurrency, per-agent usage.
- **Core**: status, version, binary, pid, port, log tail, Start / Stop / Restart.
- **Notifications** from diffs of consecutive snapshots (as the web feed): an agent stopped that the
  app did not stop, needs attention, plan limit hit and lifted, new learnings, core unreachable and
  back. Each kind can be turned off; bursts are coalesced; actions Restart and Open. Never the
  content of an agent's work.
- **Run a command…** palette (see *Command coverage*).
- **Menus and keys**: ⌘1–9 sections, ⌘O open instance, ⌘R refresh, ⇧⌘L logs, ⇧⌘P palette, agent
  actions on the selection; standard Edit and Window menus.

Done when: every row of the coverage table except Configure's works from the app through the pinned
binary, and a stopped agent raises one notification.

## Phase 3: configure

The *Configuration management* section, built: schema-driven forms, raw YAML and Markdown editors,
validation on the fields after each save, FSEvents refresh and hash-checked writes, the secrets
matrix (`init`, `set`, `unset`, `share`, `check`, `env`), new agent and new unit sheets, Slack
manifest generation, "Create local shared space", the changed-files view, and the persona editor
with a live preview (needs the office's sprite and desk drawing from Phase 4, so that one editor
lands with it). New instance (`new instance`, then `secrets init`) is part of the first run and
works from the bundled binary alone.

Done when: an agent can be created, configured (unit, model, channels, secret names and values,
SOUL), checked and started without leaving the app or touching a terminal, and the resulting files
keep their comments and pass `stormo check` from the CLI.

## Phase 4: the native office

The largest single piece. The behaviour to match is the web office (docs/core.md §3); the drawing is
new.

- **Layout** (`FloorLayout`, pure Swift, unit-tested): the same plan as `app.js` `build()`: units
  other than `group` alternate left and right of two hallways, the last office on a short side spans
  the remaining rows; each office has a two-column desk grid (at least 2 desks, padded to an even
  count with vacant desks), windows on the outer wall, a door to the hall, wall art and a plant; the
  middle column is the server room over the lobby (clocks, kitchen, water cooler, seating,
  reception, entrance, desks for `group` and unassigned agents). Output: rectangles in a logical
  coordinate space, the spots (seat, aisle, door, hall, coffee, water, sofa, window, stretch,
  entrance, robot dock and hub) and a walk graph. The view scales to fit, with zoom (pinch, ⌘+ / ⌘−)
  and scroll.
- **Playback** (`Playback`, pure): per agent, the path for its `activity`/`phase`/`slot` (the same
  routing as `pathTo`, `spotOf`, `robotRoute`) and the position at
  `f = (serverNow − start) / (end − start)`, facing from the segment direction. A new `seq` starts a
  new path; a hurried return starts from the last drawn position (as `backPath`). Under 250 ms left
  or with Reduce Motion: the end point.
- **Drawing**: `TimelineView(.animation)` + `Canvas`. The static layer (floors, walls, furniture,
  props, racks) is drawn once per layout into a cached image; the live layer draws screens (busy
  scan, code scroll, screensaver), people, the robot, the LLM traffic arcs and the clocks (night
  styling before 07:00 and from 19:00). People are ported from `charSVG`: 40×60 body, front, back and
  side views, six hair styles, four accessories, colours from the persona's `sprite:` or the same
  hash-seeded pick, limb animation (typing, idle, relax, walk, coffee, stretch, sofa) as functions of
  time. Name tags, the busy bubble, the review count and the `!` marker are SwiftUI views over the
  canvas (crisp, clickable, accessible), positioned from the playback.
- **Catalog parity**: 6 screen apps + off, 1–2 monitors, 21 props (max 6), 6 floor items, 4 floors,
  4 wall arts, 8 unit hues, the server room's racks (one fleet blade per agent), the robot and its
  notebook. A test lists the web's catalog keys against StormoKit's, so a new prop in one is noticed
  in the other.
- **Interaction**: click an agent, desk, rack or the robot → inspector; hover → label; right-click →
  the agent's actions; double-click → logs. The office can open in its own window for a second
  screen.
- **Accessibility**: `accessibilityChildren`, one element per agent ("Atlas, Working · Slack") and
  per room; Reduce Motion snaps as the web does; Increase Contrast thickens outlines.
- **Development loop**: `go run ./examples/office-demo` (port 18700, simulated activity) with a
  Debug-only core URL override; SwiftUI previews from the golden fixtures.

Done when: side by side with the web office on the same core, every agent is in the same activity
and phase at the same time, and the catalog test passes.

**As built** (SpriteKit, not `Canvas`: `Stormo/Office/`, with the pure parts in StormoKit `Office/`):

- **Distance** (`OfficeCamera`, `OfficeDetail`): the zoom stays inside what the camera can show.
  Close up the floor is as described above; further out (a smaller window, a bigger fleet) name tags
  stack and grow against the camera so they stay readable, and the one under the pointer shows in
  full; from far away each room is a tile with its people by what they are doing (`RoomSummary`),
  and a click goes into the room.
- **Input**: click → inspector; double-click a person (or Return) → its recent activity
  (`/api/agents/<id>/timeline`; the app has no logs view); arrow keys and Tab move between people;
  right-click → actions. A wheel zooms at the pointer; a trackpad moves the floor once zoomed in
  (it zooms at the whole floor, and with ⌘ held); drag pans; double-click open floor fits.
- **Counts**: a bar under the floor, each agent counted once by its mode (`FleetSummary`), "to
  review" as items. The ChatGPT plan shows only when an agent routes through the core or the plan is
  signed in or limited.
- **Timeline** (`OfficeTimelineView`, the toolbar's Floor / Timeline switch): a lane per agent,
  grouped by unit, with its stretches of work over the last 1 hour to 7 days from `GET /api/timeline`
  (`FleetTimeline`, `TimelineAxis`), the time worked, and the same inspector and activity sheet. A
  core without the route gets a note to restart it.
- **Accessibility**: the floor's children for VoiceOver are the rooms with their counts and the
  people as buttons (`OfficeAccessibleFloor`); each timeline lane reads as one line.
- **Not built**: hover labels at close range (the tag is already in full there), the office in its
  own window, Increase Contrast outlines, and the web/app catalog test.

## Phase 5: build, sign, ship

- **The bundled binary**: `go build` darwin/arm64 and darwin/amd64 with the release flags, `lipo`
  into one universal binary at `Contents/Helpers/stormo`. The app is universal too (macOS 26 still
  runs on some Intel Macs).
- **Signing**, inside out: the helper (`codesign --options runtime --timestamp`), then the app, with
  the Developer ID Application identity. Hardened runtime, no special entitlements. Local Debug
  builds sign to run locally and need none of this.
- **Notarization**: `notarytool submit --wait` with an App Store Connect API key, `stapler staple`,
  a DMG built with `hdiutil`, itself signed and notarized.
- **Credentials, all made on the web** and stored as Actions secrets in a `release` environment
  limited to `v*` tags (the repository is public; forks and pull requests never see them):
  - the Developer ID Application certificate as a `.p12` (certificate + private key). The private
    key lives where its CSR was made: export the `.p12` from that Mac's Keychain, or make a fresh
    CSR with `openssl`, upload it on developer.apple.com, and join the downloaded `.cer` and the key
    into a `.p12` with `openssl`;
  - an App Store Connect team API key (Users and Access › Integrations; Developer role is enough
    for notarization): the `.p8`, key id, issuer id;
  - the team id of the account that owns the certificate;
  - Sparkle's EdDSA private key (generated once with Sparkle's `generate_keys`; the public key goes
    in `Info.plist`).
  (Xcode cloud-managed Developer ID signing would avoid the `.p12`; whether a team API key can use it
  from `xcodebuild` in CI is unverified, so it is a spike, not the plan.)
- **Versions**: `CFBundleShortVersionString` = the tag without `v`; `CFBundleVersion` = the workflow
  run number (Sparkle compares it).
- **Updates**: Sparkle 2, feed
  `https://github.com/camfinc/stormo/releases/latest/download/appcast.xml`, the DMG and appcast as
  release assets, signed with Sparkle's `sign_update`/`generate_appcast` from the pinned Sparkle
  release. Daily check and on demand.
- **The sidecar image**: a released binary pulls `ghcr.io/camfinc/stormo-sidecar:<version>`. New
  organisation packages on GitHub start private: after the first release, set it public once in the
  package's settings.
- **Icon**: an Icon Composer `.icon` built from `docs/brand/mark-bare.svg` on the `#0d141c` tile. The
  menu bar template comes from the nine-dot `favicon.svg` cut; `tools/brandgen` gains it as one more
  generated file, so the brand stays one generator.
- **CI**: `ci.yml` gains a macOS job (`swift test`, unsigned `xcodebuild build`). `release.yml` gains
  a macOS job after the checks: build, sign, notarize, attach `Stormo-<tag>.dmg` and `appcast.xml`.
- **Docs**: README install section ("Download Stormo for macOS, or the CLI archive"), ARCHITECTURE
  layout gains `apps/macos/`.

Done when: a tagged release produces a notarized DMG that opens on a clean Mac without a Gatekeeper
warning, the previous release updates itself to it, and a fresh install starts an agent with the
bundled binary pulling the sidecar anonymously.

## Cloud, later

With cloud instances: remote status and actions (`-r`, `--target remote`), `handoff`,
`deploy render` and `render-shared`, `secrets push`, S3 nap stores for `learn -r` and `dream`, and
editing `deploy.*`. They need a confirmation the app can show natively, which the terminal prompt
(`ops.Deps.Confirm`, declining on a non-terminal) cannot give: every remote command builds its whole
step list before any side effect, `--plan` prints it with a hash and changes nothing, and
`--yes --plan-hash <h>` runs only if the list is unchanged. `handoff` needs restructuring for this
today: it stops the local container and copies naps before its remote-start confirmation.

Also later: SSE `/api/events` in the core (the app drops polling with no UI change); a
launchd-managed core (`SMAppService.agent`, needs a `core down` that knows about launchd); a
per-instance core port; Shortcuts (App Intents); a shared floor model only if a third client
appears; other platforms as their own native clients over the same contract (their engine blockers
are in docs/core.md, not in the UI).

## Security and privacy

- The app adds no HTTP surface to the core. Reads stay loopback-only and keyless, as today.
- Activity can hold client data: no API bodies in unified logging (`OSLog` with `.private` for agent
  fields), no telemetry, no crash reporter that uploads, nothing from responses persisted.
- Tokens stay where the core keeps them (`.swarm/core/auth/`); the app never reads them. Secret
  values go to the CLI on stdin and are never displayed back.
- Review reports and logs are shown, never copied out of the instance by the app.

## Owner steps

1. Export the Developer ID Application certificate as a `.p12` (or make a new one, see Phase 5),
   create the App Store Connect API key, and add both to the `release` environment's secrets.
2. Confirm the team id for the app's bundle id (register the identifier on developer.apple.com;
   Developer ID needs no provisioning profile).
3. After the first release, set the `stormo-sidecar` package public on GitHub.
4. Pushing tags (`git tag v… && git push origin v…`) stays the owner's step, as for the CLI.

## Order of work

| phase | what | rough size | blocked by |
|---|---|---|---|
| 0 | engine contract: `/health`, `/api/instance`, `api`, `--json`, config and `new` commands, `secrets set`, fixtures, docs/api.md | 1.5–2 weeks | nothing |
| 1 | app skeleton, binary and environment resolution, instances, core supervisor, menu bar, Settings | 1 week | 0 |
| 2 | operate: agents, inspector, lifecycle, logs, chat, learning, bench, review, plan, notifications, palette | 2 weeks | 1 |
| 3 | configure: forms, editors, secrets, new agent/unit, Slack manifests, first-run instance | 1.5–2 weeks | 1 (persona preview needs 4) |
| 4 | the native office | 2–3 weeks | 1 (in parallel with 2–3) |
| 5 | universal binary, signing, notarization, DMG, Sparkle, CI | 3–5 days | owner steps 1–2 |

Phases 0–2 give a working app (every local command, menu bar, notifications) before the office and
the editors land; until then the Office section can say "coming soon" rather than embed the web page.
