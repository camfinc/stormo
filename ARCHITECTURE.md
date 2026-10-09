# Stormo architecture

Stormo is an agent swarm engine. It takes an **instance** (one organisation's agent definitions:
[docs/instances.md](docs/instances.md)), compiles each agent for an engine (Hermes today,
swappable), deploys it to AWS ECS, tests it on a bench, and folds what the running agents learn
back into git through a reviewed nap/dream loop.

Engine and instance are separate repos. The engine (this repo) holds no organisation: names,
AWS target, scrub rules, bridge actions and branding come from the instance's `stormo.yaml`, and
an instance vendors the engine (docs/instances.md). Examples below use the example instance
(`examples/minimal`, the made-up org "Acme") and its placeholders: `<resource>` is
`names.resource`, `<secret>` is `names.secret`, `<state>` is `names.state_dir`, `<bucket>` is
`deploy.aws.bucket`.

## Layout

An instance (docs/instances.md):

```
stormo.yaml                    the instance: org, pinned names, AWS target, bridge module, office
units/<unit>/unit.yaml         the org's units, plus group
units/<unit>/knowledge/*.md    human-authored policy for that layer (compiled into agents)
personas/<slug>/               persona look: avatar lock, baked avatar + provenance
                               (runtime documents agents exchange live on EFS: /shared/group, /shared/<unit>)
agents/<id>/                   one folder is the whole agent (docs/agent-standard.md)
  agent.yaml                   manifest, format 1: unit, engine, model, channels, secret NAMES, non-secret env,
                               actions, schedules, state, memory, limits, learning, deploy
  SOUL.md                      behavior (the persona's look lives in personas/<slug>/)
  skills/                      Agent Skills (SKILL.md); $AGENT_HOME is the agent's home at runtime
  scripts/                     scripts schedules and skills run ($AGENT_HOME/scripts/)
  engine/<kind>/               what only that engine reads: engine/hermes/config.yaml (Hermes' config,
                               agent.yaml settings applied over it at build), engine/hermes/plugins/
  bench/*.yaml                 bench scenarios
  learnings/                   dream output: ledger.jsonl, proposals/, watermark.json, DREAM.md
  data/                        never in git: store/ (local naps), secrets.yaml (its own values), agent.env
                               (format 0 kept hermes/config.base.yaml, hermes/cron.jobs.json and plugins/;
                               `stormo migrate agent` moves them)
bridge/actions.yaml            the instance's bridge actions
dist/ .swarm/ secrets.local.yaml   generated / local runtime (gitignored)
```

The engine, one Go binary (CLI, core service and sidecars):

```
cmd/stormo/                    the CLI
pkg/
  instance/                    finds and loads the instance (stormo.yaml)
  manifest/                    agent and unit manifests, validation (unit isolation of actions), optional secrets
  engine/ engine/hermes/       Engine interface + snapshot classifier; the Hermes adapter
  build/                       agent → dist/<id>/baseline (+ .swarm/baseline.json hashes)
  learning/                    hot memory, PII scrub, ledger, compiled knowledge
  loop/                        nap store (dir or S3), snapshot, nap, rehydrate, handoff sync, dream, review
  secrets/ aws/                secrets.local.yaml layers, Secrets Manager push; the aws CLI wrapper
  deploy/                      ECS task definition + task role policy (render only)
  local/ ops/ place/           compose twin of the task, lifecycle verbs, the one-place rule
  bench/ bridge/ slack/        scenario runner; bridge action contract; Slack app manifests
  shared/                      shared document space: layers, generated shared-docs skill
  inspect/ review/             `stormo inspect` (agent as JSON), `stormo review` (fleet sweep)
  out/                         CLI output: text, or --json events (docs/api.md)
  core/ core/llm/              control plane: fleet registry, office UI (embedded), model gateway (docs/core.md)
docker/sidecar.Dockerfile      the engine's sidecar image (rehydrate + nap), one per engine version
docker/instance.Dockerfile     one agent's ECS sidecar: that image + the instance's files
apps/macos/                    the native macOS app (SwiftUI) and StormoKit, its logic as a Swift package
examples/minimal/              example instance (Acme): the test fixture and the template for new ones
examples/office-demo/          the office UI with simulated activity (`go run ./examples/office-demo`)
testdata/                      expected snapshot classes for representative runtime paths
```

## Engines are a plug, not a dependency

`agent.yaml` names `engine.kind`. An engine (`pkg/engine`) supplies: how to compile a
baseline home from the manifest, its container image/command/env/health check, its runtime layout
(where memory and skills live), and the **snapshot rules** that classify every runtime file. The
nap/dream machinery, the deployer and the bench only talk to that interface. Adding an engine is a
new module plus one registry line; agents switch by changing `engine.kind` and porting their
engine-specific files (`hermes/` today).

### Hermes specifics

- Image: stock `nousresearch/hermes-agent:<image_tag>`, pinned per agent (`engine.image_tag`).
  Upstream ships roughly weekly; never deploy `latest`.
- The compiled baseline is also a valid Hermes **profile distribution** (`distribution.yaml`,
  `.env.EXAMPLE`), so `hermes profile install dist/<id>/baseline` works for local runs.
- Overrides Stormo owns (`ApplyOverrides` in `pkg/engine/hermes`):
  - `terminal.backend: local`. Fargate has no Docker daemon, so the task itself is the sandbox.
    **Tradeoff:** a persistent Docker sandbox would isolate tool execution from the gateway; on
    ECS the isolation boundary is the task (its own IAM role, secrets and volume). Use Modal or
    Daytona backends if per-command isolation matters. No-agent cron scripts get the same env as
    the agent's terminal (Hermes strips only its own provider and messaging keys, e.g.
    `SLACK_BOT_TOKEN`), so a secret a cron script needs is within the agent's reach too.
  - `agents/<id>/scripts/` ships to `$AGENT_HOME/scripts/` (Hermes: `$HERMES_HOME`, the same place); manifest `env:` (non-secret settings,
    never `*_TOKEN`/`*_KEY`/`*_SECRET`/`*_PASSWORD`) is exported to the container under the engine's own keys.
  - Sandbox-only settings are neutralised (`docker_volumes`, `docker_forward_env`, `terminal.env`).
  - `memory.*_char_limit` comes from the manifest; `database.journal_mode: wal` (task-local disk;
    must become `delete` if the home ever moves to EFS).
  - Manifest `state:` entries become env vars pointing under
    `/opt/data/<state>/<name>` (the example's Atlas: `NEWS_RADAR_HOME`). Not `state/`: Hermes
    v0.21 keeps its own runtime state there.

## Runtime on ECS

One task per agent, desired count 1, deployed stop-then-start (`maximumPercent=100`,
`minimumHealthyPercent=0`): a Hermes gateway is a single writer per home, and two Telegram pollers
on one bot token fight.

```
                 task volume "home" (ephemeral)
                 ┌─────────────────────────────┐
  rehydrate ───▶ │ /data  ==  /opt/data         │ ◀─── agent (stock Hermes image)
  (runs once,    │ SOUL, config, skills, memory,│      gateway run, API :8642
   essential=no) │ state/, state.db, cron …     │
                 └─────────────────────────────┘
  nap  ─────────▶ snapshots every N s + on SIGTERM ──▶ s3://<bucket>/<agent>/   
```

- `agent` depends on `rehydrate: SUCCESS` and `nap: START`. Per the ECS `dependsOn` docs,
  containers stop in reverse dependency order, so the agent should stop first and `nap` take its
  final snapshot of a quiesced home (bounded to 90 s inside the 120 s `stopTimeout` cap).
  **Unverified until the first real task stop** (open decision 3). If both receive SIGTERM together,
  SQLite snapshots stay consistent (`VACUUM INTO`) but a memory or skill write could be mid-flight.
- Rehydrate writes an empty `.env` so the image does not seed its `.env.example`, whose defaults
  (`TERMINAL_TIMEOUT=60`, …) would override `config.yaml`. Secrets come only from container env.
- Secrets: one Secrets Manager secret per agent (`<secret>/<id>`), one JSON key per manifest
  secret name, injected only into the `agent` container. Sidecars get none; they use the task role,
  which can read/write only `s3://<bucket>/<id>/*`.
- `stormo deploy render <id>` writes `dist/<id>/taskdef.json` + `task-policy.json` and prints the aws
  commands. It never calls AWS.

## Operating agents

`stormo` (one Go binary; README, Install) is the single entry point (`swarm` works as a second name). Every lifecycle verb works on
**local** containers by default and on **ECS** with `--remote` / `SWARM_TARGET=remote`:

| | local (Docker compose: OrbStack, Docker Desktop, Docker Engine on Linux) | remote (ECS) |
|---|---|---|
| `swarm` | compose state + health, API port, last nap in `agents/<id>/data/store` | `describe-services`, last nap in S3 |
| `start` | build baseline, render compose, up | `update-service --desired-count 1` |
| `stop` | down (agent first, final nap), drop home volume | `--desired-count 0` (final nap on SIGTERM) |
| `restart` | stop + start: final nap → fresh home → rehydrate | `--force-new-deployment` |
| `learn` | nap now, dream from `agents/<id>/data/store` | dream from S3 |
| `handoff --to` | stop ECS (final nap) → copy naps S3 → `agents/<id>/data/store` → start | stop local → copy naps → start ECS |

An agent runs in **one place at a time** (`pkg/place`): local and ECS share its Slack app, and
Socket Mode would split its events between two live connections. `start` refuses while the other
side runs and points at `handoff`, which stops the source, checks its shutdown nap landed, copies
the latest nap (complete) plus every nap the dream has not folded (learning files only; raw
session history of older naps stays put) to the destination store, and starts there. Memory,
skills, pairings and conversation history move; credentials never do. A local start on the
production tokens checks ECS itself; `--allow-prod-token` only vouches when AWS cannot be reached.
A separate dev Slack app under `local.agents.<id>` lifts the rule for side-by-side testing. The
core drives agents through `pkg/ops` too, so the rule holds there.

Both columns show lessons and skill proposals waiting for review. Remote changes print the AWS call
and ask for confirmation (or `--yes`). They never create services; that stays an explicit
`deploy render` step.

The local project is a twin of the ECS task (`pkg/local`): same three containers and stop
order, sidecars on the engine's `stormo-sidecar:<version>` image with the instance's files and the
baseline mounted read-only (so `stormo restart` after a manifest or skill edit needs no image
build; `stormo start` builds the sidecar image itself when this engine version has none), naps in each agent's `agents/<id>/data/store` (format 0: `.swarm/store/<id>`, moved on start), shared space in
`workdir/<layer>` (gitignored; tracked by the core, docs/core.md §4), API on `127.0.0.1:18642+`. Environment comes from `secrets.local.yaml` with its
`local:` overlay. `start` refuses to run on a production bot/app token.

### Local subscription model (`model.local`)

`model.local: {via: core, name}` (format 0: `engine.local: {via: core, model}`) makes a local run use the swarm core's model gateway first: the
core holds the one ChatGPT sign-in (ChatGPT plan usage) and serves an OpenAI-compatible
`/v1/chat/completions` to every local agent. The AWS model stays as `fallback_providers[0]`.
Hermes switches to it on a 401 (core not logged in), a 429 (plan limit, held until the reported
reset) or an unreachable core, and tries the core again each turn. Only local `start`/`restart`
build this variant, into `.swarm/local/<id>/baseline`; `dist/` (which the sidecar image copies)
is always the AWS build, so the local route can never ship.

Why the core and not each agent: the plan's refresh tokens rotate on every refresh, so two holders
of one sign-in, or a restored older copy, race and lose it. The core is the only process that
holds or refreshes it; agents hold nothing but their own `SWARM_CORE_KEY` (a local-only secret:
`secrets init` mints it under `local.agents.<id>`, `secrets push` never sends it). The core is a
host process (it drives the local compose projects itself), so agent containers reach it at
`host.docker.internal:18600` (`extra_hosts: host-gateway` covers plain Docker Engine). Moving an
agent between local and ECS therefore hands over memory, skills and history and never a
credential. The sign-in is OpenAI's **Sign in with ChatGPT** with ChatGPT plan usage, OpenAI's
program for open-source, locally hosted apps (whether an instance's use qualifies is its owner's
call, see docs/core.md open decision 5): the core registers its own OAuth client
for this user, workspace and host (it shares nothing with the Codex CLI) and calls the public
Responses API. Its terms want tokens stored locally and under the user's control, which
`.swarm/core/auth/` (0600, gitignored) is. A paid or remotely hosted app would need OpenAI's
interest form, so hosted agents stay on API keys. One plan's limits (including the weekly per-app
cap the owner sets in ChatGPT) cover every local agent.

## Secrets

`secrets.local.yaml` (gitignored, 0600, checked by `stormo check`) holds every agent's secrets in
one file, layered `shared` → `units.<unit>` → `agents.<id>`, with a `local:` overlay of the same
shape for local runs. An agent receives only the names its `agent.yaml` declares. Keys several
agents use (OpenRouter, ElevenLabs, Apify) live once under `shared:`; `stormo secrets share NAME...
[--from <agent>]` moves a key there and drops identical or empty per-agent copies (a more specific
layer wins, so even an empty per-agent entry would hide the shared value). `secrets init` puts a
name two or more agents declare under `shared:`; Slack/Telegram tokens and `API_SERVER_KEY` stay
per agent.
`stormo secrets push [agent] --yes` writes each agent's set to `<secret>/<id>` in Secrets Manager.
It shows a key-name diff first and passes values to the aws CLI on stdin. On ECS the values are
injected into the agent container only.

**Optional secrets.** `optional_secrets:` in `agent.yaml` lists secrets an agent can run without, each
with the skills (under `agents/<id>/skills/`) and bridge actions it gates; a channel whose token is an
optional secret runs only when that token has a value. With no value, the build leaves those skills
out (and records them as `withheld`, so rehydrate does not bring them back from an older nap), drops
the actions and channels, and the agent starts anyway; `secrets check` reports them as
`optional off`. What counts as present: locally, the resolved local values; for AWS, the **aws layer
of `secrets.local.yaml`** (what `secrets push` sends). `deploy render` lists only present optional
secrets in the task definition, since a `valueFrom` pointing at a missing JSON key fails the task at
start. So adding an optional secret on AWS later means `secrets push`, then rebuild and redeploy.
The example's Atlas: `CRM_API_TOKEN` (its CRM skills and the deals action) and
`TELEGRAM_BOT_TOKEN` (its Telegram channel).

## Channels: Slack

Agents talk to people (and to each other) in Slack. Each agent is its own Slack app, with its own
name and the avatar baked in `personas/<slug>/`, connected with **Socket Mode**: an outbound WebSocket, so
neither the ECS task nor a laptop needs a public URL or ALB rule.

- `agent.yaml` `channels: [{kind: slack, …}]` holds the non-secret settings: `allowed_users`
  (Slack Member IDs; anyone else must be paired, and pairings persist through naps),
  `home_channel` for cron deliveries, `allow_bots` (default `mentions`: other agents may
  @mention this one, which is how handoffs between agents work without bot loops), and a `local:`
  override for a dev workspace. The engine turns them into `SLACK_ALLOWED_USERS`,
  `SLACK_HOME_CHANNEL`, `SLACK_ALLOW_BOTS`.
- Tokens are secrets: `SLACK_BOT_TOKEN` (`xoxb-`) and `SLACK_APP_TOKEN` (`xapp-`, scope
  `connections:write`). The manifest refuses a Slack channel unless both are declared.
- `stormo slack manifest <agent> [--dev]` runs the pinned image's own generator
  (`hermes slack manifest --agent-view`), so scopes, events and slash commands always match the
  deployed Hermes version, and prints the setup steps.
- One app serves both places: tokens under `agents.<id>`, and the agent is handed off between
  local and ECS (`stormo handoff`), never run in both. Socket Mode spreads events across every
  connection on an app token, so two live copies would split the traffic; `stormo start` refuses
  that. Only for testing locally while production keeps running, add a **dev app**
  (`slack manifest <agent> --dev`, "Atlas (dev)") with its tokens under `local.agents.<id>`.
- In channels Hermes answers only when @mentioned (then follows the thread); DMs always.

## Shared document space

Agents share and read documents through one EFS file system mounted into every agent container:

| mount | layer | who can read/write |
|---|---|---|
| `/shared/group` | group | every agent of the instance, all units |
| `/shared/<unit>` | that unit | that unit's agents only |

- Isolation is structural: each layer is an EFS **access point** (root `/<layer>`, uid/gid 10000 =
  the Hermes runtime user), mounted with IAM authorisation. A task only gets volumes for `group`
  and its own unit, and its task role may mount only those two access points. Cross-unit sharing
  goes through `group`.
- Hermes is told the space is writable (`HERMES_WRITE_SAFE_ROOT=/opt/data:/shared`) and where it is
  (`SWARM_SHARED_DIR`). Sidecars do not mount it.
- Every agent gets the generated shared-docs skill (`names.shared_skill`): the layout, conventions (one Markdown
  file per document with `title/author/unit/created/tags[/to]` frontmatter, handoffs under
  `<layer>/handoffs/<agent>/`, never overwrite another agent's file) and hard rules (no
  credentials; no client personal data in `group`). Its `shared_docs.py` helper (stdlib Python)
  creates date-author-slug files, so concurrent agents never collide and no locking is needed, and
  lists/filters documents (`--layer`, `--tag`, `--to <agent>`).
- The space is durable on its own, so it is outside the nap/dream loop: not snapshotted, not
  restored, never harvested into git. Documents are working material; lessons still go through
  memory and review.
- Setup: `stormo deploy render-shared` prints the file system and per-layer access point
  commands; record the ids in the instance's `stormo.yaml` (`deploy.aws.efs`). `deploy render`
  warns while they are placeholders. Locally, agents and the docker bench mount
  `workdir/<layer>` (gitignored) at the same `/shared/<layer>` paths, only group and their own unit;
  `stormo start` and `stormo shared` move an older `.swarm/shared/<layer>` there once. On the
  swarm core, agents also get locks and change attribution through it (docs/core.md §4).

## The learning system (nap / dream)

### What Hermes gives, and where it falls short for a fleet

Hermes learns in three places, all inside its home directory: `memories/MEMORY.md` + `USER.md`
(a §-delimited list capped at 2,200 / 1,375 chars, injected into every prompt), skills it creates
or patches with `skill_manage` (nudged by a background review every N turns, aged by the curator),
and session history in `state.db`. Gaps for a fleet on ECS:

1. **Ephemeral**: the task volume dies with the task. Distributions deliberately exclude memory.
2. **One writer, no merge**: two instances (or a restart) cannot merge what each learned.
3. **No review**: whatever the agent writes is live forever; nothing stops a bad lesson spreading.
4. **Tiny hot memory, no cold tier**: when the 2.2k cap fills, the agent must delete to add.
5. **No sharing**: a lesson one agent learns never reaches its teammate, let alone another unit's agent.
6. **PII**: memories and skills can quote clients; history is full of it.

### The loop

```
  running task                       S3 (per agent prefix)              git (the instance)
  ───────────                        ─────────────────────              ───────────────
  agent writes memory/skills ─nap──▶ naps/<id>.json + blobs/<sha>  ─dream─▶ learnings/ledger.jsonl
                                      raw/<sha>  (sessions, PII)      │      learnings/proposals/…
                                                                       │            │ human review
  next task boots ◀─rehydrate── baseline (image) + latest nap ◀──build── accepted learnings + skills
```

**Nap** (sidecar, every `nap_interval_seconds` and on SIGTERM). Engine rules classify each file:

| class | Hermes paths | goes to | restored on boot | reaches git |
|---|---|---|---|---|
| learning | `memories/*.md`, `skills/**`, skill telemetry, `cron/jobs.json` | `blobs/` | yes (merged) | as reviewed proposals |
| state | `<state>/**`, `local/**`, `kanban.db`, `cron/executions.db`, voice mode | `blobs/` | yes | never |
| raw | `state.db`, `response_store.db`, `sessions/**` | `raw/` | yes | never |

SQLite files are copied with `VACUUM INTO`, so snapshots are consistent while Hermes holds them
open in WAL mode. Blobs are content-addressed, so an unchanged nap costs one manifest compare.

**Rehydrate** (sidecar, once, before the engine starts) builds the home from the image baseline
plus the latest nap:
- Live memory wins over the seed, but entries a reviewer **rejected** are purged (matched by the
  ledger id of their scrubbed text). Review is enforced at runtime, not only in git.
- The compiled seed fills only `seed_fill` (default 60%) of the char budget, leaving the agent
  headroom to keep learning.
- A runtime skill patch is restored only if the repo has not changed that skill since the nap was
  taken (per-skill hashes in `.swarm/baseline.json`); otherwise the repo version wins.
  Agent-created skills always come back. Engine-bundled skills are left to the image.
- Cron: repo jobs win by id; jobs the agent created at runtime survive.
- State and raw history come back verbatim, so session search and tool state survive a redeploy.

**Dream** (`stormo dream <id>`, off-task). It reads only learning-class blobs; a guard throws if
asked for anything else, and raw history sits under a prefix the dream role can be denied in IAM.
- Every memory entry from every unseen nap is scrubbed (tokens, emails, cards, phones, plus the
  instance's own `scrub.rules`) and upserted into `learnings/ledger.jsonl` by content hash. The same lesson seen by
  several instances or naps merges into one entry with `seenCount`, `instances` and `naps`
  (provenance and corroboration).
- Changed or new skills in the newest nap become scrubbed copies under
  `learnings/proposals/skills/` (a skill already decided at that content hash is not re-proposed).
  Schedule changes the agent made at runtime become `learnings/proposals/schedules.yaml` (agent.yaml's
  form; format 0, or a job agent.yaml cannot express: `learnings/proposals/cron/jobs.json`). Skill telemetry is kept in
  `learnings/skill-usage.json`.
- It writes the working tree only. It never commits, branches or pushes.

**Review** (`stormo learn …`): accept, reject, edit (rewrite to drop PII), pin, promote; accept or
reject skill proposals. Promotion widens a lesson from `agent` to `unit` or `group`. User-profile
memories and anything PII-flagged can never be promoted, and a lesson must be accepted first.

**Build** compiles accepted learnings into two tiers:
- **hot**: the top-ranked own lessons (pinned, then most corroborated, then most recent) packed into
  the seed share of `MEMORY.md` / `USER.md`;
- **cold**: every visible accepted lesson plus `units/<unit>/knowledge/*.md` and
  `units/group/knowledge/*.md`, shipped as the generated knowledge skill (`names.knowledge_skill`; no size cap; the
  agent loads it on demand).

Visibility is the isolation rule: an agent sees its own lessons, `unit`-scoped lessons from agents
in its unit, and `group`-scoped lessons from everyone. Never another unit's unit-scoped lessons.

**Bench gate**: after accepting proposals, `stormo bench <id> --runner docker` runs the scenarios
against the rebuilt baseline in the pinned engine image before the change is committed and deployed.

## Bridge: actions and triggers

Agents should act on their organisation's systems through the bridge rather than holding broad credentials.
`pkg/bridge` defines the contract, and the instance declares its actions in `bridge/actions.yaml`: a unit-owned action
(`<unit|group>.<system>.<resource>.<verb>`, JSON-Schema input in MCP `inputSchema` shape, the
secret names the bridge needs, and `mutates`). An agent gets only the actions its manifest lists,
and only from its unit or the group; the check runs at build and again at call time. Each action
is a declarative HTTP call (`http:` method, url, bearer secret, query from the input).

Planned shape (not built yet):
- **Actions**: a `<resource>-bridge` ECS service serving the registry as an MCP server over HTTP,
  added to each agent's `mcp_servers` with a per-agent key. Mutating actions go through an
  approval path.
- **Triggers**: EventBridge Scheduler / webhooks → SQS → bridge → the agent's API server
  (`POST /v1/runs` on :8642, `API_SERVER_KEY`). This replaces ad-hoc crons for cross-system events
  (an SLA breach in a support inbox, a new order). Agent-internal schedules stay in Hermes cron.

## Core (control plane)

`swarm-core` is the fleet's control plane, local first: a Go service on the host
(`127.0.0.1:18600`, reached from agent containers as `host.docker.internal:18600`) that monitors
every agent, shows live status and current work in a web UI, carries agent-to-agent messages,
tracks attribution and locks in the shared `workdir/`, drives the nap/dream learning cycle and
checks agents for non-compliance. Locally it is also the single holder of the ChatGPT-subscription
login and serves model calls to the other agents. A `core` Hermes agent in `units/group` sits on
top of it later. Plan, phases and open decisions: [docs/core.md](docs/core.md).

## Open decisions

1. **Who runs the dream**: by hand (`stormo learn`), or by the swarm core on a schedule the
   instance opts into (`core.learning.at` in stormo.yaml; docs/core.md §6), which naps and dreams
   into the working tree only and never commits. Still open: a scheduled job that opens a PR,
   which means creating branches and needs the instance owner's sign-off.
2. **LLM consolidation in the dream**: merging near-duplicate lessons and generalising incident
   notes into rules. The ledger and review flow are ready for it; the model call is not written.
3. **Verify on an instance's first deploy**: ECS shutdown order (agent before nap), the first real
   S3 nap and rehydrate, and the docker bench runner against the real model.
4. **Engine-bundled skills patched at runtime** are dropped on rehydrate by design (the image owns
   them). If an agent keeps improving a bundled skill, fork it into `agents/<id>/skills/`.
5. **Deployment choices left to each instance** (its own docs): Fargate or EC2 (`deploy.launch`),
   GHCR or ECR (`deploy.registry`), networking for webhook triggers, the nap bucket's encryption,
   versioning and the `*/raw/` lifecycle rule and dream-role deny, and backups for the shared space.
