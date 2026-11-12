# Stormo's agent standard

Status: **built** (Phases 0–2 below): `format: 1`, `stormo migrate agent`, `stormo export` and
`stormo import`. The example instance is format 1. Not built: `tools:` (the neutral subset of
Hermes' config beyond limits:; nothing else varies per agent today), MCP tool names in generated
skills (`mcp_core_*` is Hermes' naming). This page was written as the plan, so "today" below means
before format 1; where the build differs, docs/instances.md and `stormo --help` are current.

### Migrating an instance's live agents

A format-1 manifest needs a binary that knows format 1, and so do the running core and agents.

1. `cd stormo && go build -o bin/stormo ./cmd/stormo` (and rebuild the app if it is used).
2. `stormo stop` (all agents), then restart the core on the new binary (`stormo core down && stormo core up`).
3. `stormo migrate agent` (every agent; agents stopped, so their naps and their own secret values
   move into agents/<id>/data/ too; secrets.local.yaml is rewritten without its comments: back it up).
4. Review `git diff` (agent.yaml, engine/hermes/, skills and scripts) and commit in the instance.
5. `stormo start`.

Owner decisions recorded under *Decided*.

The owner's direction: agents are defined in Stormo's own format, engine-neutral, in one directory
that holds everything an agent is (config, behaviour, skills, assets, state, memory); the active
engine (Hermes today) gets it rendered into the shape it expects. An agent can be exported as a
self-contained zip, as **config** only or **config + data** (secrets, env, state, memory), and
imported into another instance.

## Where we are: how Hermes-specific it is today

The `engine.Engine` interface (`pkg/engine/engine.go`) covers compiling the baseline home and the
container (image, env, command, port, health, bench). Everything else assumes Hermes:

| area | today | verdict |
|---|---|---|
| agent.yaml identity, unit, role, persona, secrets (names), env, actions, state, deploy | Stormo's | neutral |
| `engine.version`, `image_tag` | Hermes app version and image `nousresearch/hermes-agent` | Hermes-shaped (fine as the adapter's settings) |
| `engine.model`, `provider` | written into Hermes `model.default/provider`; provider defaults to openrouter | neutral idea, Hermes default |
| `engine.local {via: core}` | Hermes `fallback_providers`, hooks, `mcp_servers` | neutral idea, Hermes implementation |
| `channels` (slack, telegram) | validated by Stormo, run by the Hermes gateway (env only); `slash_commands` are Hermes' | Hermes-implemented |
| `learning.memory_char_limit`, `user_char_limit` | Hermes' MEMORY.md / USER.md budgets, 1:1 | Hermes |
| `agents/<id>/hermes/config.base.yaml` | the whole Hermes config, ~600 lines, `_config_version: 46` | Hermes |
| `agents/<id>/hermes/cron.jobs.json` | Hermes' runtime jobs file, committed with runtime fields (`last_run_at`, `failure_streak`) | Hermes; Stormo has no schedule concept |
| `plugins/` | Hermes plugins | Hermes |
| skills | open Agent Skills frontmatter + `metadata.hermes`; skills and scripts hard-code `$HERMES_HOME/scripts` | mostly neutral, paths leak |
| memory | `Layout.Memory{Memory,User}` and `learning.EntryDelimiter "\n§\n"` in the neutral interface | Hermes in the interface |
| naps | neutral envelope, but file paths are Hermes-home-relative and classified by Hermes rules (`testdata/classify.json`) | Hermes content |
| outside the adapter | rehydrate merges Hermes cron, parses `.bundled_manifest`, seeds `.env`; the dream reads Hermes cron; review reads `/opt/data/cron/jobs.json`; the core calls Hermes' HTTP API (`/health/detailed`, `/api/sessions`, `/api/jobs`, `/v1/runs`, `X-Hermes-Signature-256`); `API_SERVER_KEY`; generated skill templates write `metadata.hermes` | leaks |

So: the manifest is mostly neutral already; the agent directory, memory, cron, naps and the
core's runtime calls are Hermes. No export/import exists.

## Decided / proposed

| | proposal | |
|---|---|---|
| format | `agent.yaml` stays the entry point, gains neutral sections (below) and `format: 1` | proposed |
| engine specifics | `agents/<id>/engine/<kind>/` holds what the standard does not express (Hermes config overrides, plugins); the adapter merges it last | proposed |
| rendering | the adapter **renders** the standard into the engine's home (as `Compile` does now); parts an engine reads as they are (skills, scripts, state dirs, assets) are copied or mounted unchanged. The agent folder itself is never mounted as the engine home: Hermes needs its own layout | proposed |
| data location | **one folder per agent, data included** (`agents/<id>/data/`, kept out of git by a `.gitignore` stormo writes inside it), so an agent can live isolated in any target directory | decided (owner) |
| secrets in exports | a config + data export **carries secret values**, in plain form, with a disclaimer: `DATA-EXPORT-CONTAINS-SECRETS.txt` in the zip, a field in `stormo-agent.json`, a warning from the CLI and a confirmation in the app | decided (owner) |
| nap paths | keep them engine-home-relative and record the engine in the nap manifest (no re-rooting, no migration) | proposed |

### The agent folder

One folder is the whole agent: definition and data. The data half is never committed: stormo
creates `data/.gitignore` (`*`) and refuses to export or import a folder whose data is tracked by
git.

```
agents/<id>/
  agent.yaml                    identity, model, channels, secrets (names), env, actions, state,
                                schedules, memory, learning, deploy
  SOUL.md                       behaviour (every engine gets it as its rules)
  skills/<cat>/<name>/SKILL.md  Agent Skills format; $AGENT_HOME, never $HERMES_HOME
  scripts/  assets/             files skills and schedules use, as they are
  bench/                        evals
  learnings/                    ledger, watermark, proposals (scrubbed; reviewed in git)
  engine/hermes/                overrides only the Hermes adapter reads (config, plugins)
  data/                         never in git (data/.gitignore)
    store/                      the local nap store (was .swarm/store/<id>)
    secrets.yaml                this agent's own secret values (shared and unit layers stay in
                                secrets.local.yaml; an export flattens them in)
    agent.env                   the rendered env file (was .swarm/env/<id>.env)
```

The persona is referenced (`persona: {path: personas/<slug>}`) and copied into an export. On AWS
the nap store stays S3 (`s3://<bucket>/<id>/`); `data/store` is the local store.

### Secrets in a config + data export

The zip carries every value the agent resolves (shared, unit and agent layers, `local:` overlay
included) in `data/secrets.yaml`, with `DATA-EXPORT-CONTAINS-SECRETS.txt` at its root and
`"containsSecrets": true` in `stormo-agent.json`. The CLI prints the warning; the app asks for
confirmation. Import writes the values to the agent's own layer and leaves a name that already has
a shared or unit value alone. A config-only export carries no value and no data.

## The four things "state and memory" mean today

| what | where | export tier |
|---|---|---|
| `state:` dirs (e.g. a spend ledger) | engine home `<state_dir>/<name>`, kept by naps | config + data |
| engine memory (Hermes MEMORY.md, USER.md, sessions) | engine home, kept by naps | config + data (latest complete nap) |
| naps | `agents/<id>/data/store` (was `.swarm/store/<id>`) or `s3://<bucket>/<id>` | config + data: latest complete; older ones learning-only (`loop.SyncNaps` already splits these) |
| learnings ledger, watermark, proposals | `agents/<id>/learnings/` (git) | config (scrubbed, already in git) |

## The standard's new neutral sections

- `schedules:` replaces `engine/hermes/cron.jobs.json`: `{id, every|cron, prompt, skills, script,
  deliver: home|<channel>, enabled}`. No runtime fields (`last_run_at` and the rest live in the
  engine home). The Hermes adapter renders `cron/jobs.json` and merges runtime state as rehydrate
  does now.
- `memory: {budget: {agent, user}}` replaces `learning.memory_char_limit/user_char_limit` (the old
  keys keep working as aliases for one release). The adapter maps budgets to its own memory files.
- `model:` neutral: `{name, provider, local: {via: core, name}}` (the current `engine.model`,
  `provider`, `local` move up; `engine:` keeps `kind`, `version`, `image`).
- `tools:` the neutral subset of `config.base.yaml` the owner actually sets (toolsets, approvals,
  timezone, limits); whatever else stays in `engine/hermes/config.yaml` as overrides.
- `channels:` unchanged in shape; secret env names become adapter-owned (`ChannelSecrets` moves
  into the adapter).

## Phases

### Phase 0: pull the leaks behind the interface (formats unchanged)

No migration, no owner decision. New `Engine` methods: memory layout and serialization (replacing
`Layout.Memory` and `EntryDelimiter`), schedule render/merge/diff (`MergeCron`, the dream's cron
reads, review's jobs path), engine-owned skills (call `EngineOwnedSkills`), home dirs to reset,
pre-boot fixups (`.env`), runtime API (health, activity, jobs, wake, chat, ingest signature, API
key name), skill template frontmatter and path variable. `pkg/core`, `pkg/loop`, `pkg/review`
call those instead of Hermes paths.

Done when: `grep -rn hermes pkg cmd` outside `pkg/engine/hermes` finds only the registry.

### Phase 1: the standard and the Hermes adapter

`format: 1`, `schedules:`, `memory:`, `model:`, `tools:`, `engine/<kind>/` overrides,
`$AGENT_HOME`; the adapter renders all of it; `config show/apply` and the app's form learn the new
sections. `stormo migrate agent <id>` converts an agent (cron.jobs.json → schedules without runtime
fields, `hermes/` → `engine/hermes/`, learning budgets → memory, `$HERMES_HOME` → `$AGENT_HOME` in
skills and scripts) and proves itself by compiling the same baseline before and after (a byte diff
of `dist/<id>/baseline`, ignoring known renames). An instance's agents migrate one commit each,
`stormo check` and the baseline diff as the gate.

### Phase 2: export and import

- `stormo export <agent> [--data] [-o file.zip]` (`--data` carries the secret values):
  `stormo-agent.json` (format, engine kind and version, mode, source instance slug, exported at,
  file hashes), `agent/` (the definition), `persona/`, `unit.json` (the unit's id, name and the
  knowledge files it relies on, for reference), `actions.yaml` (the bridge actions it uses), and
  with `--data`: `data/store/` (FsStore layout: latest complete, learning-only older),
  `data/secrets.yaml` (resolved values) and the disclaimer. Never `.swarm/secrets.local.yaml.bak`,
  `dist/`, raw sessions outside the latest nap.
- `stormo import <file.zip> [--as <id>] [--unit <u>]`: checks the format, refuses to overwrite an
  agent without `--replace`, remaps id and unit, adds missing bridge actions only with
  `--with-actions` (they are instance-wide), copies the persona if absent, loads naps into the
  local store, adds secret names (`secrets init`), runs `check`.
- The app: Export… and Import… on the agents table, a sheet for the mode.

Done when: an agent exported from one instance imports into a fresh `new instance` and starts with its
memory, and a config-only export carries no data and no secret.

## Persisted contracts this touches

| contract | effect |
|---|---|
| nap store layout and manifests | avoided: paths stay engine-home-relative; the manifest gains `engine` (optional field, old naps read as hermes) |
| `testdata/classify.json` | unchanged (the rules stay the Hermes adapter's) |
| `baseline.json` skill hashes | moving skills is not planned; `$AGENT_HOME` rewrites change hashes once, so rehydrate keeps the repo versions on that boot (as today for any skill edit) |
| ledger lines, watermarks | unchanged (`learnings/` stays) |
| secrets file shape | `secrets.local.yaml` keeps its layers; an agent's own layer can also live in `agents/<id>/data/secrets.yaml` (read as the `agents.<id>` layer; both set is an error). Migration: `stormo migrate agent` moves the agent's layer |
| local nap store `.swarm/store/<id>` | moves to `agents/<id>/data/store` (same FsStore layout); `migrate agent` moves it, and the old path is read as a fallback for one release |
| `names.state_dir`, `knowledge_skill`, `shared_skill` (pinned) | unchanged |
| `hermes/cron.jobs.json` | needs a migration (Phase 1): dream proposals that reference it are re-pointed at `schedules:` |
| `.swarm/ports.json` | unchanged |
