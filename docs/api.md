# Stormo's machine interfaces

What other programs (the macOS app, scripts, a second UI) can rely on: the core's HTTP API and the
CLI's `--json` output. Both are versioned together by `api` (`version.API` in `pkg/version`), an
integer bumped on a breaking change; added fields and new events do not bump it. Read `api` first
and refuse what you do not understand.

## The core's HTTP API

`stormo core` listens on `127.0.0.1:$SWARM_CORE_PORT` (18600). Reads need no key: the bind is the
boundary. Agent containers reach the core through bridge listeners that serve only `/health` and
`/v1/*`, so nothing under `/api` reaches them.

| route | body |
|---|---|
| `GET /health` | `status`, `service` (`swarm-core`), `login` (`ok` \| `missing` \| `relogin_required`), `planLimitedUntil`, `version`, `api`. Also on bridge listeners: no host paths |
| `GET /api/core` | the running core: `service`, `version`, `api`, `pid`, `exe` (symlinks resolved), `port`, `startedAt` (RFC 3339), `instance` {`root`, `name`, `org`, `slug`}. Tells a client which binary and which instance it would attach to |
| `GET /api/instance` | the instance's look: `name`, `org`, `slug`, `clocks` [{`city`, `tz`}], `units` {unit: {`hue`, `wall`, `floor`, `desk`}} (the web office's `window.STORMO`) |
| `GET /api/fleet` | `polledAt`, `units`, `agents` (each with its `office` timeline), `now` (server ms), `robot`, `gateway`. See docs/core.md §3 for the office fields |
| `GET /api/gateway` | the model gateway: login, `account`, plan limit, `manageUsageUrl`, in flight, queued, concurrency, `models`, per-agent `usage` and `active` |
| `GET /avatars/<id>.png` | the agent's newest portrait, 404 when it has none |
| `/v1/*` | the OpenAI-compatible model gateway, `Authorization: Bearer <SWARM_CORE_KEY>` |

Errors are `{"error":{"code","message"}}` with an HTTP status. There is no event stream yet: poll
`/api/fleet` (the web office polls every 2.5 s) and correct for clock skew with `now`.

## `--json`

Any command takes `--json`: stdout becomes one JSON object per line, each with an `event` first.

```
{"event":"step","msg":"…"}                 progress, the line text mode would print
{"event":"result","data":{…}}             what the command produced: the last line on success
{"event":"error","msg":"…","code":"…"}   why it failed: the last line on failure
```

With `--json`, stdout carries only events: anything else the command or the tools it runs print
(compose, docker, the text it would otherwise show) goes to stderr, which a caller can keep as a log.

Exit status: 0 success, 1 failure, 2 usage. Error codes: `usage` (bad flags or arguments),
`instance` (no instance found, or its stormo.yaml is invalid), `failed` (anything else). Commands
gain structured `result` data one by one; until a command has it, `--json` still turns its failure
into an `error` event.

| command | `result.data` |
|---|---|
| `version` | `version`, `api`, `released` (a tagged release, whose sidecar image is published), `exe` |
| `instance` | `root`, `name`, `org`, `slug` of the instance this invocation found (`--instance`, `STORMO_INSTANCE`, the working directory, the binary's directory, in that order) |
| `status` (`ps`) | one row per agent: `agent`, `unit`, `where`, `state` (running \| stopped \| starting \| missing \| unknown), `health`, `endpoint`, `lastNap`, `pendingLearnings`, `pendingSkills`, `detail` |
| `start`, `stop`, `restart`, `nap-now` | `action`, `where`, `agents` (in the order handled); progress as `step` events |
| `core up` | `started` (false when a core already answered), `pid`, `port`, `log`, `login` |
| `core down` | `stopped`, `pid`; not stopped: `reason` `not_running`, or `not_ours` (a core answers that `core up` did not start; left alone) |
| `core status` | `running`, `port`, `login` (the gateway's, else the sign-in on disk), `gateway` (as `/api/gateway`) when running |
| `core login` | `login`, `account`. Emits `{"event":"auth_url","url":…}` with the sign-in page; with `--no-open` it does not open a browser, the caller does |
| `core logout` | `login` (`missing`), `changed` (tokens were removed) |
