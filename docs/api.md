# Stormo's machine interfaces

What other programs (the macOS app, scripts, a second UI) can rely on: the core's HTTP API and the
CLI's `--json` output. Both are versioned together by `api` (`version.API` in `pkg/version`), an
integer bumped on a breaking change; added fields and new events do not bump it. Read `api` first
and refuse what you do not understand.

## The core's HTTP API

`stormo core` listens on `127.0.0.1:$SWARM_CORE_PORT` (18600). Reads of the fleet and the office
need no key. The bind is not a full boundary: on OrbStack and Docker Desktop agent containers reach
loopback ports through `host.docker.internal`. So reads that return message bodies, activity
previews or file history need `Authorization: Bearer <owner token>` (the value in
`.swarm/core/owner.token`, 0600, minted when the core starts; containers never mount `.swarm/core`)
or the agent's own `SWARM_CORE_KEY`, which sees only its own part. On Linux, agent containers reach
the core through bridge listeners that serve only `/health`, `/v1/*`, `/mcp` and `/ingest/*`.

| route | body |
|---|---|
| `GET /health` | `status`, `service` (`swarm-core`), `login` (`ok` \| `missing` \| `relogin_required`), `planLimitedUntil`, `version`, `api`. Also on bridge listeners: no host paths |
| `GET /api/core` | the running core: `service`, `version`, `api`, `pid`, `exe` (symlinks resolved), `port`, `startedAt` (RFC 3339), `instance` {`root`, `name`, `org`, `slug`}. Tells a client which binary and which instance it would attach to |
| `GET /api/instance` | the instance's look: `name`, `org`, `slug`, `clocks` [{`city`, `tz`}], `units` {unit: {`hue`, `wall`, `floor`, `desk`}} (the web office's `window.STORMO`) |
| `GET /api/fleet` | `polledAt`, `units`, `agents` (each with its `office` timeline, `live`: the tool it runs now, from its hooks, or null, `messages`: `unread`, `received`, `sent` today, and `locks`: shared-space locks it holds), `now` (server ms), `robot`, `gateway`. See docs/core.md §3 for the office fields |
| `GET /api/agents/<id>/timeline` | `agent`, `live`, `entries` [{`at`, `event`, `tool`}], newest first; `?limit=` (≤ 500, default 100) |
| `GET /api/agents/<id>/activity` | the same with each entry's `session` and `preview`. Owner token or that agent's key |
| `POST /ingest/<engine>` | an engine's hook deliveries (Hermes: `/ingest/hermes`, its outbound hooks), signed with the agent's core key and read by that engine's runtime (`engine.Runtime`; docs/core.md §2) |
| `GET /api/messages` | `threads` [{`thread`, `subject`, `updated`, `messages` [{`id`, `thread`, `from`, `to`, `subject`, `body`, `priority`, `attach`, `hop`, `sent`, `recipients` [{`agent`, `read`, `acked`, `woken`}]}]}], newest thread first; `?agent=`, `?limit=` (default 30). Owner token |
| `GET /api/findings` | `findings` [{`id`, `agent`, `rule`, `severity`, `evidence`, `firstSeen`, `lastSeen`, `count`, `status`}]; `?all=1` adds resolved ones. Owner token |
| `GET /api/workdir` | `locks` [{`owner`, `path`, `kind`, `reason`, `created`, `expires`}], `changes` [{`path`, `at`, `kind`, `agent`, `how`, `violation`}] newest first (`?limit=`, default 50), `layers` {layer: files}. Owner token |
| `GET /api/learning` | `at`, `timezone`, `next` (the schedule, empty when off), `running` (cycle id or null), `cycles` [{`id`, `trigger`, `started`, `finished`, `status`, `digest`, `runs` [{`agent`, `started`, `finished`, `napped`, `napNote`, `naps`, `newLearnings`, `resighted`, `skillProposals`, `cronProposal`, `pendingLearnings`, `pendingSkills`, `error`}]}], newest first. Counts only |
| `POST /api/learn` | body `{agents, force}`; 202 `{cycle}`, 409 while a cycle runs. Owner token |
| `GET /api/review` | `autoAccept`, `autoAcceptMinSeen`, `autoRestart`, `agents` [{`id`, `name`, `unit`, `state`, `busy`, `unapplied`, `lessons` [{`id`, `kind`, `scope`, `text`, `pii`, `status`, `seenCount`, `firstSeen`, `lastSeen`, `decidedAt`, `note`, `autoAcceptable`}], `skills` [{`skill`, `pii`, `nap`, `new`, `files` [{`path`, `size`, `text`, `current`}]}]}]. Owner token |
| `POST /api/review` | `{agent, lessons: [id…] \| skill, decision: accept \| reject \| promote, scope: unit \| group}`; 400 with the reason when refused. Owner token |
| `POST /api/agents/<id>/restart` | restart a running local agent (applies decisions); 409 when it is not running. Owner token |
| `GET /review` | the review page (no data; reads the owner token from `#t=`) |
| `POST /mcp` | the agents' MCP server (docs/core.md §4, §5), each agent with its own core key |
| `GET /api/gateway/models?connection=<name>` | `connection`, `models` [{`id`, `display_name`, `context_length`}]: that ChatGPT sign-in's plan catalog, fetched when the cached one is older than 10 min (the configured defaults before sign-in) |
| `GET /api/gateway` | the model gateway: login, `account`, plan limit, `manageUsageUrl`, in flight, queued, concurrency, `models` (all of the default ChatGPT connection), per-agent `usage` and `active` (across connections), and `connections` [{`name`, `login`, `account`, `planLimitedUntil`, `inflight`, `queued`, `concurrency`}], one per ChatGPT connection |
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

`--json` is recognised anywhere on the command line, also as a message or `--text` value, so pass
such values through a file or stdin when they could be the literal `--json`. With `--json`, stdout
carries only events: anything else the command or the tools it runs print
(compose, docker, the text it would otherwise show) goes to stderr, which a caller can keep as a log.

Exit status: 0 success, 1 failure, 2 usage. Error codes: `usage` (bad flags or arguments),
`instance` (no instance found, or its stormo.yaml is invalid), `failed` (anything else); `config`
adds `unsupported`, `conflict`, `invalid` and `readonly` (below). Commands
gain structured `result` data one by one; until a command has it, `--json` still turns its failure
into an `error` event.

| command | `result.data` |
|---|---|
| `version` | `version`, `api`, `released` (a tagged release, whose sidecar image is published), `exe` |
| `new instance <dir>` | `root`, `name`, `org`, `slug`, `template` (`empty` \| `example`), `git` (initialised). Runs outside any instance; a folder that exists must be empty |
| `new agent` (spec on stdin) | `id`, `name`, `unit`, `files` (written), `secrets` (declared names), `missing` (declared names with no local value). The agent's generated keys (engine API key, local-only core key) are minted into its `data/secrets.yaml`; `secrets.local.yaml` is not rewritten. The spec is JSON: `name` (required), `id` (default: slugged name), `role`, `unit` or `newUnit` {`id`,`name`,`description`}, `persona`, `engine` {`kind`,`version`,`imageTag`}, `model` {`name`,`provider`,`localName`,`localConnection`}, `channels` [{`kind`,`allowedUsers`,`homeChannel`,`allowBots`}], `secrets` (extra names), `soul` (SOUL.md; default a starter). Empty fields borrow the instance's most common engine and model. Channel, connection and engine keys are declared automatically; secret values never go in the spec (`secrets set`). Refused (`invalid`, nothing left behind) when the id is taken or the agent would not load |
| `new agent --options` | `options` (as `config show`'s, instance-wide), `defaults` {`unit`,`engine`,`model`} a new agent starts from, `takenIds` |
| `instance` | `root`, `name`, `org`, `slug` of the instance this invocation found (`--instance`, `STORMO_INSTANCE`, the working directory, the binary's directory, in that order) |
| `status` (`ps`) | one row per agent: `agent`, `unit`, `where`, `state` (running \| stopped \| starting \| missing \| unknown), `health`, `endpoint`, `lastNap`, `pendingLearnings`, `pendingSkills`, `detail` |
| `start`, `stop`, `restart`, `nap-now` | `action`, `where`, `agents` (in the order handled); progress as `step` events |
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
| `config show <file>` | `path` (instance-relative), `kind` (`agent` \| `soul`), `agent`, `hash` (sha256 of the bytes, hex), `text`; for an `agent.yaml` also `doc` (the file as JSON; absent when it does not parse) and `options`, the choices a form offers: `units` [{`id`, `name`, `description`}], `actions` [{`name`, `unit`, `description`, `mutates`}] (an agent may use its unit's and `group`'s), `skills` (the agent's, for optional secrets), `personas` (`personas/<slug>`), `engines`, `channels` [{`kind`, `secrets`}], `allowBots`, `secrets` (every name declared in the instance; never values), `scripts` (the agent's scripts/), `reasoning` (the levels its engine accepts), `connections` [{`name`, `kind`, `label`, `api`, `key`}] |
| `connections [list]` | one row per connection: `name`, `kind`, `label`, `api`, `baseUrl`, `key` (the secret's NAME), `implicit`, `keySet` (a shared value exists), `agentKeys` (agents with their own value), `usedBy` |
| `connections models <name> [--agent id]` | the models a connection offers: `id`, `name`, `contextLength`. An API is asked with its key (the agent's value with `--agent`, else the shared one; OpenRouter's catalog is public); a ChatGPT sign-in's come from the running core. Code `unavailable` when it cannot be asked |
| `connections kinds` | the kinds a connection can be: `kind`, `label`, `baseUrl` and `key` (its defaults), `api` |
| `connections add <name> --kind k [--base-url u] [--key NAME]`, `connections remove <name>` | the connections after the change. Codes: `invalid`, `in_use` (an agent uses it) |
| `secrets set <shared\|agent> NAME` | `scope`, `name`. The value comes on stdin (one line), never in argv, and is never echoed |
| `config write <file> --if-hash <h>` | as `show`, for what was written. The new content comes on stdin |
| `migrate agent [agent…]` | one row per agent: `agent`, `from`, `to` (agent.yaml formats), `changes` (lines), `data` (`moved`, `in-place`, or `running`: its data stays until it is stopped). Runs `check` after |
| `export <agent> [--data] [-o file]` | `path`, `agent`, `mode` (`config` \| `data`), `files`, `naps`, `containsSecrets`. A data export carries every secret value the agent resolves in plain text: the zip is written 0600, holds `DATA-EXPORT-CONTAINS-SECRETS.txt`, and a `step` warns. Codes: `legacy` (format 0: migrate first), `tracked` (its data folder is in git) |
| `import <file.zip> [--as id] [--unit u] [--replace] [--with-actions]` | `agent`, `from` (the source instance's slug), `mode`, `changes`. Every member is checked against the zip's manifest; the result must pass `check`, else nothing is kept. Codes: `invalid`, `exists` (pass `--replace` or `--as`), `unknown_unit`, `missing_actions` (pass `--with-actions`), `tracked` |
| `config apply <file> --if-hash <h>` | as `show`, for what was written. A JSON merge patch (RFC 7386) on stdin, `agent.yaml` only |

`config` edits `agents/<id>/agent.yaml` and `agents/<id>/SOUL.md` (`pkg/config`); any other path
is refused with `unsupported`. `write` replaces the file atomically, and only when it still hashes
to `--if-hash` (else `conflict`: reload and edit again) and the new content validates: an
`agent.yaml` must load as the manifest and its bridge actions do in `check` (else `invalid`, with
the reason) and keep `deploy:` as it was (else `readonly`: it names cloud resources); a `SOUL.md`
must not be empty. Building the agent is not part of the write: run `check <agent>` after it.

`apply` is how a form saves: it edits the file's YAML node tree, so a key the patch does not name
keeps its comments and place, and the file is re-encoded with its original blank lines and comment
alignment wherever nothing changed (an empty patch changes no byte). Merge patch semantics: `null`
deletes a key, an object merges into an object, anything else (an array too) replaces the value;
inside a replaced array, unchanged strings keep their comments and objects are matched by position.
A patch that names `id`, `deploy` or `format` is refused with `readonly` (`stormo migrate agent` changes the format); then the result is validated and
written as `write` does.
