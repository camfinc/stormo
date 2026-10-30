# Stormo's machine interfaces

What other programs (the macOS app, scripts, a second UI) can rely on: the core's HTTP API and the
CLI's `--json` output. Both are versioned together by `api` (`version.API` in `pkg/version`), an
integer bumped on a breaking change; added fields and new events do not bump it. Read `api` first
and refuse what you do not understand.

## The core's HTTP API

`stormo core` listens on the instance's core port (`127.0.0.1`, 18600 for the first instance on a
host; docs/core.md). **Every route except `/health` needs a credential** (docs/remote-core.md §3):

- a person: `Authorization: Bearer <token>`, either the owner token (`.swarm/core/owner.token`,
  0600, minted when the core starts, scope admin; containers never mount `.swarm/core`) or a
  personal token from `stormo core token add` (`stc_…`, scope `read`, `operate` or `admin`,
  optionally expiring, revocable). GET needs `read`, anything that changes state `operate`,
  `/api/tokens` and `/api/audit` `admin`. A token without the scope gets 403, no token 401.
- an agent: its own `SWARM_CORE_KEY` on `/v1/*`, `/ingest/*`, `/mcp` and its own
  `/api/agents/<id>/activity`, nothing else.

While `core.loopback_trust` is on (the default), a request that reaches the loopback listener
directly (no `X-Forwarded-For`/`Forwarded`) still reads the views that were always open locally
without a token: `/`, `/review`, `/ui/*`, `/api/core`, `/api/instance`, `/api/fleet`,
`/api/gateway[/models]`, `/api/learning`, timelines and avatars. On OrbStack and Docker Desktop agent
containers reach loopback ports through `host.docker.internal`, so nothing that holds client data is
among them. On Linux, agent containers reach the core through bridge listeners that serve only
`/health`, `/v1/*`, `/mcp` and `/ingest/*`.

| route | body |
|---|---|
| `GET /health` | `status`, `service` (`swarm-core`), `login` (`ok` \| `missing` \| `relogin_required`), `planLimitedUntil`, `version`, `api`. Also on bridge listeners: no host paths |
| `GET /api/core` | the running core: `service`, `version`, `api`, `pid`, `exe` (symlinks resolved), `port`, `startedAt` (RFC 3339), `instance` {`root`, `name`, `org`, `slug`}. Tells a client which binary and which instance it would attach to |
| `GET /api/instance` | the instance's look: `name`, `org`, `slug`, `clocks` [{`city`, `tz`}], `units` {unit: {`hue`, `wall`, `floor`, `desk`}} (the web office's `window.STORMO`) |
| `GET /api/fleet` | `polledAt`, `units`, `agents` (each with `pendingLearnings`, `pendingSkills`, `pendingChanges` (runtime schedules and settings proposals), its `office` timeline, `live`: the tool it runs now, from its hooks, or null, `messages`: `unread`, `received`, `sent` today, `locks`: shared-space locks it holds, and `usage`: `cpuPercent`, `memBytes`, `memLimitBytes` of its local containers together from `docker stats`, absent when not running), `now` (server ms), `robot`, `gateway`. See docs/core.md §3 for the office fields |
| `GET /api/agents/<id>/timeline` | `agent`, `live`, `entries` [{`at`, `event`, `tool`}], newest first; `?limit=` (≤ 500, default 100) |
| `GET /api/agents/<id>/activity` | the same with each entry's `session` and `preview`. Owner token or that agent's key |
| `POST /ingest/<engine>` | an engine's hook deliveries (Hermes: `/ingest/hermes`, its outbound hooks), signed with the agent's core key and read by that engine's runtime (`engine.Runtime`; docs/core.md §2) |
| `GET /api/messages` | `threads` [{`thread`, `subject`, `updated`, `messages` [{`id`, `thread`, `from`, `to`, `subject`, `body`, `priority`, `attach`, `hop`, `sent`, `recipients` [{`agent`, `read`, `acked`, `woken`}]}]}], newest thread first; `?agent=`, `?limit=` (default 30). Owner token |
| `GET /api/findings` | `findings` [{`id`, `agent`, `rule`, `severity`, `evidence`, `firstSeen`, `lastSeen`, `count`, `status`}]; `?all=1` adds resolved ones. Owner token |
| `GET /api/workdir` | `locks` [{`owner`, `path`, `kind`, `reason`, `created`, `expires`}], `changes` [{`path`, `at`, `kind`, `agent`, `how`, `violation`}] newest first (`?limit=`, default 50), `layers` {layer: files}. Owner token |
| `GET /api/workdir/layers` | `layers`: `group` first, then each unit an agent of the roster belongs to. Owner token |
| `GET /api/workdir/ls?path=/shared/<layer>/…` | `path`, `entries` [{`name`, `dir`, `size`, `modified`, `lastWriter`, `locked`}]; links are skipped. Owner token |
| `GET /api/workdir/info?path=` | `path`, `exists`, `dir`, `size`, `modified`, `creator`, `lastWriter`, `how`, `locks`, `history` (fs_info's answer). Owner token |
| `GET /api/workdir/file?path=` | `path`, `size`, `modified`, `truncated` (past 1 MiB), `binary`, `text` (absent when binary). Read-only, never follows a link out of its layer; a bad path is a 400. Owner token |
| `GET /api/learning` | `at`, `timezone`, `next` (the schedule, empty when off), `running` (cycle id or null), `cycles` [{`id`, `trigger`, `started`, `finished`, `status`, `digest`, `runs` [{`agent`, `started`, `finished`, `napped`, `napNote`, `naps`, `newLearnings`, `resighted`, `skillProposals`, `cronProposal`, `pendingLearnings`, `pendingSkills`, `error`, `settingProposals`, `autoAccepted` (approved by the review), `passed` (passed to a person), `held` (held by the rules), `reviewNote`, `restarted`, `restartNote`}]}], newest first. Counts only |
| `POST /api/learn` | body `{agents, force}`; 202 `{cycle}`, 409 while a cycle runs. Owner token |
| `GET /api/review` | `review` (`auto` \| `manual`), `reviewConnection`, `reviewModel`, `reviewPrompt`, `autoAccept` (review is auto), `autoAcceptMinSeen` (a lesson is judged once seen this many times), `autoAcceptMinConfirmations` (deprecated, unused), `autoRestart`, `agents` [{`id`, `name`, `unit`, `state`, `busy`, `unapplied`, `autoReview`, optional `notices` (problems reading its learning state that fail nothing, e.g. an unreadable `evidence-v1.json`, which reads as no evidence), `lessons` [{`id`, `kind`, `scope`, `text`, `pii`, `status`, `seenCount`, `firstSeen`, `lastSeen`, `decidedAt`, `note`, `autoAcceptable`, `evidence` ({`revision`, `unknown`, `attempts`, `confirmations`, `failures`, `corrections`, `evidenceTypes`, optional `validatedAt`}: what the agent's runtime recorded about this exact text, empty `evidenceTypes` when nothing was; shown to the review's model, never a rule), `ai`, `held`}], `skills` [{`skill`, `pii`, `nap`, `new`, `stale`, `base`, `files` [{`path`, `size`, `text`, `current`}], `evidence` (same aggregate fields as lessons), `ai`, `held`}], `schedules` (null, or {`jobs` (agent.yaml schedules), `runtimeJobs`, `scripts` [{`name` (path in `agents/<id>/scripts/`), `status` (`new` \| `changed`), `stale` (the repo changed it since; accept refused), `size`, `text`, `current`}] (scripts the agent wrote for them; an accept writes them to `agents/<id>/scripts/`), `pii`, `nap`, `ai`, `held`}), `settings` [{`key`, `value`, `current`, `sensitive`, `opaque`, `pii`, `nap`, `ai`, `held`}]}]. On an open proposal `ai` is the review's verdict on its current content ({`approve`, `reason`, `model`, `at`}) and `held` why the rules keep it for a person. Owner token |
| `GET /api/agents/<id>/learning[?session=<id>]` | Owner learning bridge: `{state, message?, sessions, session?, data?}`. Without a session, lists selectable conversations; with a session, returns its events, candidates and budget/status. Read token; never available through loopback trust or an agent key |
| `POST /api/agents/<id>/learning?session=<id>` | Either `{eventId, outcome, origin, summary}` or `{exportId}`; same response envelope. Operate token; audited without source text or credentials. The runtime owner credential is resolved server-side, never accepted in this body or query |
| `POST /api/review` | `{agent, lessons: [id…] \| skill \| schedules: true \| settings: [key…], decision: accept \| reject \| promote (lessons), scope: unit \| group}`; 400 with the reason when refused. Owner token |
| `POST /api/agents/<id>/restart` | restart a running local agent (applies decisions); 409 when it is not running. Owner token |
| `POST /api/agents/<id>/start\|stop\|restart` | `{placement, confirm}` (placement: as asked, else where its link runs, else local). local runs compose here; remote (ECS) answers 428 `{plan}` until `confirm: true`; a runner placement goes to its runner (503 when none serves it). → `{action, agent, placement, log, state}`. Operate; audited |
| `GET /api/agents/<id>/slack[?refresh=1]` | what the agent's Slack bot sees, for pickers instead of ids: `team` {`id`, `name`}, `bot` (its own user), `people` [{`id`, `name` (display, else real name), `realName`, `handle`, `bot`, `guest` (restricted or single-channel), `owner` (owner or admin), `tz`}] (people first, then bots; deleted members left out), `channels` [{`id`, `name`, `private`, `member` (the bot is in it)}] (public ones, and private ones it is in), `dms` [{`id`, `user`}] (DMs it already has), `fetchedAt`, `partial` (lists a missing scope left empty). Read with the agent's own bot token as a local run sees it, whatever its engine; cached in memory 5 min (`refresh=1` reads again), never stored. 404 `no_slack` when it has no Slack channel or bot token, 502 `slack_<code>` when Slack refuses (`slack_invalid_auth`: the token is wrong), 429 with `Retry-After` when rate limited. Owner token; agent keys are refused |
| `POST /api/agents/<id>/slack/dm` | `{user: "U…"}` → `{channel, user}`: the bot's DM with that person (Slack's `conversations.open`: opens it when missing, posts nothing), the `D…` id a home channel or a `slack:<id>` delivery takes. Owner token (operate) |
| `GET /api/placements` | `placements` [{`name`, `kind`: local \| ecs \| runner, `available`}]. Read |
| `GET /link` (WebSocket) | an agent's live wire (pkg/link): hello, status, wake/nap commands. The agent's core key |
| `GET /runner` (WebSocket) | a runner's link: lifecycle orders for its placement. An operate token |
| `GET /api/capabilities` | `capabilities` [{`name`, `description`, `kind` (http \| container \| service), `template`, `placement`, `concurrency`, `timeoutMinutes`, `costUSD`, `files` (none \| in \| out \| both), `docs`, `inputSchema`, `examples`, service settings (`port`, `health`, `idleMinutes`, `bootMinutes`, `memoryMB`, `shmMB`, `cpus`, `env`), `queued`, `running`, `agents` (granted), `worker` (asleep \| booting \| awake for a service here, runner, absent when each job starts its own), `service` {`state`, `since`, `lastUsed`, `running`, `error` (why its last boot failed)}}], `spend` [{`agent`, `capability`, `jobs`, `failed`, `usd`}] this month, `since`. Read |
| `POST /api/capabilities/<name>/wake\|sleep` | boot a service capability's worker now (in the background) / stop it; 409 when it is not a service on this host or (sleep) jobs run in it. → `{capability, service}`. Operate; audited |
| `GET /api/capabilities/jobs[?limit=n]` | `jobs` [{`job`, `capability`, `agent`, `status`, `error`, `preview` (scrubbed), `costUSD`, `created`, `started`, `finished`, `inputs` (shared paths handed over), `out` (the folder its files go to), `files` (what it wrote), `notify`, `uploads`, `pending`}], never inputs' contents or outputs. Read |
| `POST /v1/capabilities/jobs/<id>/files?name=<file>` | the body is one file for the caller's own job waiting for uploads (`cap_start` `files_pending`), streamed into the job's folder (never the shared space); the last one queues the job. → the job (`status`, `uploads`, `pending`). 409 when the job is not the caller's, waits for no files, already has that name, or the name is not a plain file name. The agent's core key |
| `GET /api/capabilities/jobs/<id>` | one job for the owner: as in the list, plus `output` (scrubbed as previews are, cut at 64 KB with a `note`). 404 for no such job. Read |
| `POST /api/capabilities/jobs/<id>/cancel` | the owner's cancel: a queued or waiting job never runs, a running one is stopped; `error` "cancelled by the owner", and its agent gets the notice when it asked for one. → the job. Operate; audited |
| `/api/sync/<layer>[/<path>]` | a runner's sync (operate token, `X-Stormo-Placement`): GET lists `files` [{`path`, `sha256`, `size`}] or reads one; PUT/DELETE with `X-Base-SHA256` (409 stale, 423 locked). Only group and its agents' units |
| `GET /api/tokens[?all=1]` | `tokens` [{`id`, `name`, `scope`, `created`, `expires`, `lastUsed`, `revoked`}], never values. Admin |
| `POST /api/tokens` | `{name, scope: read \| operate \| admin, days}` (0 days never expires) → 201 `{token, info}`; the value is shown this once. Admin |
| `DELETE /api/tokens/<name or id>` | revokes at once. Admin |
| `GET /api/audit[?limit=n]` | `entries` [{`at`, `actor` (owner, a token's name, `agent:<id>`), `action`, `target`, `detail`}], newest first. Admin |
| `GET /review` | the review page (no data; reads the owner token from `#t=`). Only with `core.web_ui: true`, like `/` and `/ui/*`; otherwise 404 |
| `POST /mcp` | the agents' MCP server (docs/core.md §4, §5), each agent with its own core key |
| `GET /api/gateway/models?connection=<name>` | `connection`, `models` [{`id`, `display_name`, `context_length`}]: that ChatGPT sign-in's plan catalog, fetched when the cached one is older than 10 min (the configured defaults before sign-in) |
| `GET /api/gateway` | the model gateway: login, `account`, plan limit, `manageUsageUrl`, in flight, queued, concurrency, `models` (all of the default ChatGPT connection), per-agent `usage` and `active` (across connections), and `connections` [{`name`, `login`, `account`, `planLimitedUntil`, `inflight`, `queued`, `concurrency`}], one per ChatGPT connection |
| `GET /avatars/<id>.png` | the agent's newest portrait, 404 when it has none |
| `/v1/*` | the OpenAI-compatible model gateway, `Authorization: Bearer <SWARM_CORE_KEY>` |

Errors are `{"error":{"code","message"}}` with an HTTP status. There is no event stream yet: poll
`/api/fleet` (the web office polls every 2.5 s) and correct for clock skew with `now`.

### Runtime learning bridge

The browser and native app use the core bridge, never the runtime API directly. The engine's
optional `LearningRuntime` interface supplies the route and dedicated secret name. For needletail,
the core resolves the agent's declared `NEEDLETAIL_LEARNING_KEY` and ordinary conversation key
from local secrets (including the local overlay). They must be distinct usable credentials.
Neither value reaches clients, query parameters or audit records. Requests do not follow redirects;
responses redact exact configured secret values. Session titles/previews and selected learning data
pass through memory to the authorized caller and are not persisted by this bridge.

Availability failures use HTTP 200 with `state`: `ready`, `unsupported` (engine has no learning
interface), `unavailable` (offline, missing session/API, invalid response or transport failure),
`missing_credential` (missing, unusable or refused dedicated/conversation credential), or `error`
(runtime refused the operation). `message` is a safe description, never an upstream error body.
Malformed input remains HTTP 400; ordinary core authentication/scope failures use 401/403.
Only one `session` query selector is accepted. POST bodies are at most 4,096 bytes; feedback
summaries are at most 2,000 bytes. Unknown fields, trailing JSON and mixed feedback/export bodies
are refused before calling the runtime.

`data` is present after a successful selected-session request:

- `events`: outcome metadata for the selected conversation and authorized audience, including
  deliberately agent-scoped events from that task. Views and command exits do not confirm success.
- `candidates`: private reflection review material (`id`, `text`, `checks`, `conditions`, `exceptions`,
  `audience`, `sensitivity`, `reviewDate`, `action`, `relatedIds`, `eventRefs`, `root`, and optional
  `previousMistake`). Export requires an agent-wide public/procedural candidate; export proposes
  material for dream/review and does not activate it.
- `status`: `reflectionEnabled`, `callsUsed/callsLimit` (100 lifetime reservations),
  `eventsUsed/eventsLimit` (4,096), `candidatesUsed/candidatesLimit` (100),
  `exportsUsed/exportsLimit` (100), `exportedIds`, `feedbackAllowed`, `exportAllowed`.
  Counts describe store capacity; exported identities are limited to candidates visible in this
  selected task. Disabled reflection still permits feedback. Untrusted conversations cannot write.
- Optional `operation`: `{ok, code, message, feedbackSaved}`. A feedback write can succeed before
  reflection fails: HTTP 200 and `state: ready` then carry `ok: false`, `code: reflection_failed`,
  and `feedbackSaved: true`. Display that partial result and refresh; do not report that feedback
  was lost or automatically resubmit it. Export success has `feedbackSaved: false`. A transport
  failure also requires a refresh before retrying, because a write may already have reached the runtime.

## `--json`

Any command takes `--json`: stdout becomes one JSON object per line, each with an `event` first.

```
{"event":"step","msg":"…"}                 progress, the line text mode would print
{"event":"result","data":{…}}             what the command produced: the last line on success
{"event":"error","msg":"…","code":"…"}   why it failed: the last line on failure
```

`--json` is recognised anywhere on the command line, also as a message or `--text` value, so pass
such values through a file or stdin when they could be the literal `--json`. With `--json`, stdout
carries only events: anything else the command or the tools it runs print
(compose, docker, the text it would otherwise show) goes to stderr, which a caller can keep as a log.

Exit status: 0 success, 1 failure, 2 usage. Error codes: `usage` (bad flags or arguments),
`instance` (no instance found, or its stormo.yaml is invalid), `failed` (anything else); `config`
adds `unsupported`, `conflict`, `invalid` and `readonly` (below); `chat` and `conversations` add
`not_running` (the agent's engine API does not answer here), `busy` (the engine refuses another turn
now) and `not_found` (no such conversation). Commands
gain structured `result` data one by one; until a command has it, `--json` still turns its failure
into an `error` event.

| command | `result.data` |
|---|---|
| `version` | `version`, `api`, `released` (a tagged release, whose sidecar image is published), `exe` |
| `new instance <dir>` | `root`, `name`, `org`, `slug`, `template` (`empty` \| `example`), `git` (initialised). Runs outside any instance; a folder that exists must be empty |
| `new agent` (spec on stdin) | `id`, `name`, `unit`, `files` (written), `secrets` (declared names), `missing` (declared names with no local value). The agent's generated keys (engine API key, local-only core key) are minted into its `data/secrets.yaml`; `secrets.local.yaml` is not rewritten. The spec is JSON: `name` (required), `id` (default: slugged name), `role`, `unit` or `newUnit` {`id`,`name`,`description`}, `persona` (an existing `personas/<slug>`) or `look` (a new one, written to `personas/<id>`; shape as `look show`'s `look`; refused when that folder exists), `engine` {`kind`,`version`,`imageTag`}, `model` {`name`,`provider`,`localName`,`localConnection`}, `channels` [{`kind`,`allowedUsers`,`homeChannel`,`allowBots`}], `secrets` (extra names), `soul` (SOUL.md; default a starter). Empty fields borrow the instance's most common engine and model. Channel, connection and engine keys are declared automatically; secret values never go in the spec (`secrets set`). Refused (`invalid`, nothing left behind) when the id is taken or the agent would not load |
| `new agent --options` | `options` (as `config show`'s, instance-wide), `defaults` {`unit`,`engine`,`model`} a new agent starts from, `takenIds` |
| `instance` | `root`, `name`, `org`, `slug` of the instance this invocation found (`--instance`, `STORMO_INSTANCE`, the working directory, the binary's directory, in that order) |
| `status` (`ps`) | one row per agent: `agent`, `unit`, `where`, `state` (running \| stopped \| starting \| missing \| unknown), `health`, `endpoint`, `lastNap`, `pendingLearnings`, `pendingSkills`, `pendingChanges` (runtime schedules and settings proposals), `detail` |
| `start`, `stop`, `restart`, `nap-now` | `action`, `where`, `agents` (in the order handled); progress as `step` events |
| `chat <agent> <msg>\|- [--session s]` | `agent`, `session` (where the conversation is now: an engine compaction can move it), `reply`. `-` reads the message from stdin (use it: a message could be `--json`). The session (default `swarm-local`) is created when missing. A turn can take minutes; cancel by ending the process |
| `conversations <agent>` | `agent`, `conversations` [{`id`, `title`, `source` (`api_server`, `slack`, `cron`, …), `preview`, `startedAt`, `lastActive`, `messages`, `ended`, `compacted`}], most recently active first. `id` stays the conversation's first session across automatic compactions; counts and times are the live part's. Titles, previews and messages are client data: shown to the owner, never sent to the core |
| `conversations <agent> <session>` | `agent`, `id`, `tip` (the session holding it now), `compacted` (older turns were folded into a summary by the engine's automatic compaction), `messages` [{`id`, `role`, `kind` (`text` \| `tool`), `content`, `at`, `tools` (an assistant row's tool calls), `tool` (the tool a tool row answers), `source` (`follow_up`: a user row no one wrote, the input of a turn the agent's own follow-up started; absent otherwise)}], oldest first, the latest 500 |
| `conversations new <agent> [--name title]` | as a transcript, empty: `id` of the new conversation |
| `conversations clear <agent> <session>\|--all` | `agent`, `cleared` (ids), `removed` (sessions, compaction continuations included). `--all` clears the conversations started through the API (`source` `api_server`); a Slack thread's or a schedule's stays with its channel |
| `core up` | `started` (false when a core already answered), `pid`, `port`, `log`, `login` |
| `core down` | `stopped`, `pid`; not stopped: `reason` `not_running`, or `not_ours` (a core answers that `core up` did not start; left alone) |
| `core status` | `running`, `port`, `login` (the gateway's, else the sign-in on disk), `gateway` (as `/api/gateway`) when running |
| `core login [--connection name]` | `login`, `connection`, `account`. Emits `{"event":"auth_url","url":…}` with the sign-in page; with `--no-open` it does not open a browser, the caller does |
| `core logout [--connection name]` | `login` (`missing`), `connection`, `changed` (tokens were removed) |
| `core activity <agent> [n]` | `agent`, `live`, `entries` (as `/api/agents/<id>/activity`, newest first) |
| `core messages [agent] [n]` | `threads` (as `/api/messages`) |
| `core findings [all]` | `findings` (as `/api/findings`) |
| `core workdir [n]` | as `/api/workdir` |
| `core learn [agent…] [--force]` | the finished cycle (as in `/api/learning`); a `step` per agent while it runs |
| `core learning` | as `/api/learning` |
| `core review [--no-open]` | `url`; emits `auth_url` with the page address, owner token in the fragment (keep it private) |
| `check [agent…]` | one row per agent that passed: `agent`, `unit`, `engine`, `files`, `skills`, `format` (its agent.yaml format) and `legacy` (format-0 keys it still uses) (each also a `step`). The build goes to `.swarm/check/<id>`; a failing agent ends the command with an `error` |
| `config show <file>` | `path` (instance-relative), `kind` (`agent` \| `soul` \| `unit` \| `knowledge`), `agent` (empty for a unit's files), `unit` (`unit` and `knowledge` only), `hash` (sha256 of the bytes, hex), `text`; for an `agent.yaml` or a `unit.yaml` also `doc` (the file as JSON; absent when it does not parse); for an `agent.yaml` also `options`, the choices a form offers: `units` [{`id`, `name`, `description`}], `actions` [{`name`, `unit`, `description`, `mutates`}] (an agent may use its unit's and `group`'s), `skills` (the agent's, for optional secrets), `personas` (`personas/<slug>`), `engines`, `channels` [{`kind`, `secrets`}], `allowBots`, `secrets` (every name declared in the instance; never values), `scripts` (the agent's scripts/), `reasoning` (the levels its engine accepts), `connections` [{`name`, `kind`, `label`, `api`, `key`}] |
| `look show <agent>` | `agent`, `path` (its persona folder, empty when it has none), `hash` (of persona.md, for `look set`), `look` {`description` (the `## Avatar lock` text), `sprite` {`skin`,`hair`,`hair_style`,`shirt`,`pants`,`accessory`}, `desk` {`apps` (one per monitor), `screens`, `props` (≤ 6), `side`}}, `editable` (false for a persona in a sibling repo, with `reason`), `shared` (other agents with the same persona) |
| `look set <agent> [--if-hash h]` (look JSON on stdin) | as `look show`, after the write. Only the sprite, desk and Avatar lock change; other frontmatter, comments and text stay. An agent with no persona gets `personas/<agent>/persona.md` and its agent.yaml points there (no hash needed). Codes: `invalid`, `conflict` (stale hash, or `personas/<agent>` exists), `readonly` (sibling repo) |
| `connections [list]` | one row per connection: `name`, `kind`, `label`, `api`, `baseUrl`, `key` (the secret's NAME), `implicit`, `keySet` (a shared value exists), `agentKeys` (agents with their own value), `usedBy` |
| `connections models <name> [--agent id]` | the models a connection offers: `id`, `name`, `contextLength`. An API is asked with its key (the agent's value with `--agent`, else the shared one; OpenRouter's catalog is public); a ChatGPT sign-in's come from the running core. Code `unavailable` when it cannot be asked |
| `connections kinds` | the kinds a connection can be: `kind`, `label`, `baseUrl` and `key` (its defaults), `api` |
| `connections add <name> --kind k [--base-url u] [--key NAME]`, `connections remove <name>` | the connections after the change. Codes: `invalid`, `in_use` (an agent uses it) |
| `secrets set <shared\|agent> NAME` | `scope`, `name`. The value comes on stdin (one line), never in argv, and is never echoed |
| `secrets status <agent>` | one row per name the agent receives: `name`, `kind` (`required` \| `optional` \| `local`: local-only, e.g. its core key), `source` (the layer with a non-empty value: `agent` \| `unit` \| `shared`, empty when missing), `local` (the `local:` overlay layer overriding it for local runs, or empty). Never values |
| `config write <file> --if-hash <h>` | as `show`, for what was written. The new content comes on stdin |
| `migrate agent [agent…]` | one row per agent: `agent`, `from`, `to` (agent.yaml formats), `changes` (lines), `data` (`moved`, `in-place`, or `running`: its data stays until it is stopped). Runs `check` after |
| `export <agent> [--data] [-o file]` | `path`, `agent`, `mode` (`config` \| `data`), `files`, `naps`, `containsSecrets`. A data export carries every secret value the agent resolves in plain text: the zip is written 0600, holds `DATA-EXPORT-CONTAINS-SECRETS.txt`, and a `step` warns. Codes: `legacy` (format 0: migrate first), `tracked` (its data folder is in git) |
| `delete agent <agent> [--with-persona] --yes` | `agent`, `removed` (what went), `notes` (what stays: a persona another agent uses, a stopped ECS service, an emptied unit). A `step` per item. Stops a local container first; refused (`refused`) while the agent runs on ECS. Without `--yes` it asks on a terminal |
| `import <file.zip> [--as id] [--unit u] [--replace] [--with-actions]` | `agent`, `from` (the source instance's slug), `mode`, `changes`. Every member is checked against the zip's manifest; the result must pass `check`, else nothing is kept. Codes: `invalid`, `exists` (pass `--replace` or `--as`), `unknown_unit`, `missing_actions` (pass `--with-actions`), `tracked` |
| `config apply <file> --if-hash <h>` | as `show`, for what was written. A JSON merge patch (RFC 7386) on stdin, `agent.yaml` and `unit.yaml` only |
| `config create <file>` | as `show`, for what was written: a new `units/<unit>/unit.yaml` (a new unit; `<unit>` a slug) or `units/<unit>/knowledge/<name>.md` of an existing unit, from stdin. Code `exists` when the file is there |
| `config delete <file> --if-hash <h>` | `path`. Removes a unit's knowledge file; nothing else is deleted here |
| `config units` | one row per unit, `group` first: `id`, `name`, `description`, `path` (its unit.yaml), optional `problem` (why it does not load), `agents` (those its knowledge reaches: its members, every agent for `group`), `knowledge` [{`path`, `name`, `size`, `compiled` (false for `README.md`, which is for people)}] |
| `capability show` | `path` (`stormo.yaml`), `hash` (for `--if-hash`), `capabilities` [{`name`, `spec` (the stanza as written, as JSON), `effective` (as it runs: template defaults filled, as in `/api/capabilities`, plus `unavailable` when this engine cannot run it), `agents` (granted)}] |
| `capability set <name> --if-hash <h>` | as `show`, after the change: a JSON merge patch (RFC 7386) on stdin for that capability's stanza (made when missing; `null` removes a setting), comments elsewhere kept. The whole file is checked before it is written. Codes: `conflict`, `invalid` (it would not load, or names a template this engine does not ship), `usage`. The core loads it at its next restart |
| `capability remove <name> --if-hash <h>` | as `show`, after the change. Codes: `in_use` (an agent is granted it), `not_found`, `conflict` |

`config` edits `agents/<id>/agent.yaml`, `agents/<id>/SOUL.md`, `units/<unit>/unit.yaml` and
`units/<unit>/knowledge/<name>.md` (`<name>` letters, digits, `.`, `_`, `-`; `pkg/config`); any
other path is refused with `unsupported`. `write` replaces the file atomically, and only when it still hashes
to `--if-hash` (else `conflict`: reload and edit again) and the new content validates: an
`agent.yaml` must load as the manifest and its bridge actions do in `check` (else `invalid`, with
the reason) and keep `deploy:` as it was (else `readonly`: it names cloud resources); a `SOUL.md`
must not be empty; a `unit.yaml` must keep `id` as its folder's name and have a `name` (else
`invalid`). Building the agent is not part of the write: run `check <agent>` after it (for a unit's
files, on the agents `config units` lists for it: its knowledge is compiled into them).

`apply` is how a form saves: it edits the file's YAML node tree, so a key the patch does not name
keeps its comments and place, and the file is re-encoded with its original blank lines and comment
alignment wherever nothing changed (an empty patch changes no byte). Merge patch semantics: `null`
deletes a key, an object merges into an object, anything else (an array too) replaces the value;
inside a replaced array, unchanged strings keep their comments and objects are matched by position.
A patch that names `id`, `deploy` or `format` (a `unit.yaml`: `id`) is refused with `readonly` (`stormo migrate agent` changes the format); then the result is validated and
written as `write` does.

### Background learning review metadata

Runtime learning review data may include an additive `background` object:
`enabled`, `pending`, `completed`, `failed`, `interrupted`, `callsUsed`,
`tokensReserved`, and `candidates`. Job counts and candidates match the selected
task root and authorized audience; budget counters cover the whole agent home over
the rolling 24-hour window. Candidates remain private review material and cannot
be passed to the legacy `exportId` operation. The existing dedicated owner
credential, core authorization, redaction, and no-store response policy apply.
Clients that do not render this optional field continue to work.


The optional `background` object also includes `jobs`, `callsLimit` and `tokensLimit`.
Each job exposes its ID, kind, state, attempts, owner retries, timestamps, next attempt,
bounded result code and candidate ID. `skills` contains patches (before/after text,
base/current revisions, status, checks, conditions, evidence references and action
eligibility), maintenance recommendations, archives, last scan and timer enabled status.
Responses are bounded to 64 MiB, 256 jobs/patches/archives, 4,352 retained background
candidates and 4,096 maintenance rows, and recursively redacted by the core.

POST to the existing owner learning endpoint accepts these additive control requests:

| `action` | Additional fields |
|---|---|
| `prune_jobs`, `scan_maintenance` | None |
| `cancel_job`, `retry_job`, `reject_patch`, `restore_skill` | `id` |
| `approve_patch`, `rollback_patch`, `archive_skill` | `id`, `expectedRevision` |

For archive, `id` is the skill name; restore uses the archive receipt ID. Patch and job
operations use their respective IDs. Control fields cannot mix with feedback or export
fields. Operations retain the selected root/audience authority, quarantine checks and
owner authentication. Safe failures include `revision_conflict`, `protected_skill`,
`disabled` and `retry_limit`. An operation result does not claim feedback was saved.
Cancellation invalidates an in-flight generation; retry can spend another bounded model
reservation. Pruning preserves candidates, dedupe identities and rolling costs.
