# Stormo core: plan

Status: local first. **Phase 1 (the LLM gateway, on Sign in with ChatGPT) is built** (`pkg/core/`,
`stormo core up|down|status|serve|login|logout`). **The office UI over a polled fleet registry is
built** (`pkg/core/fleet.go`, `pkg/core/ui/`, part of phase 2), and so is `core.db`
(`pkg/core/db.go`). **Phase 3 (activity ingest) is built** (`pkg/core/monitor.go`). The Phases table
says what each phase has; everything not marked built is design.
AWS comes later and is sketched only where it changes a local decision.

Core is the fleet's control plane. It watches every agent, shows what each one is doing, drives
the learning phase (naps, dream, review queue), checks agents for non-compliance, carries messages
between agents, and owns the shared working directory with attribution and locks. Locally it also
holds the one ChatGPT-subscription login and serves model calls to the other agents.

## Two parts: a service and an agent

| | `swarm-core` **service** | `core` **agent** |
|---|---|---|
| what | deterministic Go process (`pkg/core/`) | a Hermes persona in `units/group` (`agents/core/`) |
| does | gateway, registry, health polling, activity ingest, comms bus, workdir locks, nap/learn scheduling, rule-based compliance checks, web UI | judgement: LLM compliance review of samples, daily fleet digest, triage of findings, talks to people in Slack |
| needs an LLM | no (it *serves* one) | yes, through the gateway locally |
| can change production | never | never (alerts and suggests the `swarm` command; the owner runs it) |

The service is useful without the agent; the agent is just another client of the service's MCP
tools, with more permissions (fleet-wide reads, nap triggers). Build the service first.

## Local topology

```
 host (macOS with OrbStack / Docker Desktop, or Linux)             gitignored
 ┌──────────────────────────────────────────────┐
 │ swarm-core  (go,  127.0.0.1:18600)           │  .swarm/core/  core.db, auth/chatgpt.json,
 │  /v1/*      LLM gateway (ChatGPT plan)       │                agent-keys, core.log, pid
 │  /mcp       comms + workdir + fleet tools    │  workdir/      shared files (all agents)
 │  /ingest/*  Hermes outbound hook events      │
 │  /api/*     UI + CLI JSON API, /events SSE   │
 │  /          web UI                           │
 └───────▲──────────────────────────────────────┘
         │ http://host.docker.internal:18600   (on Linux via the docker0 bridge listener)
 ┌───────┴─────────────┐  ┌─────────────────────┐
 │ swarm-atlas         │  │ swarm-<agent> …     │   one compose project per agent (unchanged)
 │  agent  :8642 → 18642│  │                     │   + /workdir bind mount
 │  nap  (trigger :8650)│  │                     │   + mcp_servers.core, hooks.outbound → core
 └─────────────────────┘  └─────────────────────┘
```

Why a host process and not a container:
- it drives the local lifecycle exactly like the `swarm` CLI does (`pkg/ops`: compose, `NapNow`,
  `learn`), with no Docker socket mounted into anything;
- it reads `secrets.local.yaml` directly, so per-agent keys need no rendered copy;
- containers reach a loopback-bound host port through `host.docker.internal` on OrbStack and
  Docker Desktop. Docker Engine on Linux resolves that name (the agent's
  `extra_hosts: host.docker.internal:host-gateway`) to the docker0 bridge address, which loopback
  never sees, so on Linux the core also listens on docker0's IPv4, same port (`pkg/core/bridge.go`).
  That bridge listener serves only `/v1/*` (each call keyed by `SWARM_CORE_KEY`) and `/health`; the
  office, `/api/*` and avatars stay on loopback. `SWARM_CORE_BIND` overrides the addresses
  (comma-separated, e.g. a custom `host-gateway-ip` in daemon.json or a rootless setup) and `none`
  turns the listener off. A host firewall (ufw, firewalld) must let the Docker networks reach
  that port. It serves what agents call, each keyed or signed per agent: `/v1/*`, `/mcp`,
  `/ingest/*`, and `/health`.

**Lifecycle rule.** Core starts, stops, restarts and moves agents only through `pkg/ops`
(`start`, `stop`, `restart`, `handoff`), never raw `docker compose up` or ECS `update-service`.
Those paths carry the one-place guard (`GuardStart` in `pkg/place`): an agent has one Slack
app used locally and on ECS, and runs in exactly one place at a time, since two Socket Mode
connections would split its events. Remote start/stop always goes through `OpsDeps.confirm`,
which in core asks the owner; core never answers it on the owner's behalf. Status reads use
`localSide` / `remoteSide` from `pkg/place`. Nap triggers (`napNow`) exec into a running
sidecar and start nothing, so they are outside the rule.

On AWS (later) the same code runs as an ECS service; only the lifecycle backend changes to the
`remote` branch of `pkg/ops`, and the gateway is switched off (see Phase 1, scope).

`stormo core up` starts it in the background (pid and log in `.swarm/core/`), `stormo core down`
stops it, `stormo core serve` runs it in the foreground. A launchd plist can come later.

## Identity: one key per agent

Every call into core carries `Authorization: Bearer $SWARM_CORE_KEY`. The value is per agent,
stored under `local.agents.<id>.SWARM_CORE_KEY` in `secrets.local.yaml`; core maps key → agent on
each request (reloading the file when it changes), so the caller's identity comes from the key, never
from a field the agent fills in. One key covers the gateway, MCP, ingest and API.

- `stormo secrets init` mints it for every agent with `engine.local.via: core` (`LocalOnlySecrets` in
  `pkg/manifest`); the core reloads `secrets.local.yaml` when it changes, so no restart is needed.
- The key is **local-only**: it must never be pushed to Secrets Manager. The manifest needs a
  local-only secret notion (agent-side work, see Handoffs).
- The web UI is bound to loopback and needs no key from a browser on this machine; `/api/*` from a
  container needs an agent key and only exposes what that agent may see.

## 1. LLM gateway (Phase 1, built)

Core holds the one ChatGPT sign-in and serves model calls to every local agent. The sign-in is
OpenAI's **Sign in with ChatGPT** with ChatGPT plan usage (developers.openai.com/siwc), OpenAI's
program for open-source, locally hosted apps; whether an instance's use qualifies is its owner's
decision (open decision 5). Its refresh tokens rotate, so exactly
one process holds and refreshes them: core.

- Agent side (unchanged contract): `POST /v1/chat/completions` (streaming and not), `GET /v1/models`,
  `GET /health`, each agent with its own `SWARM_CORE_KEY`.
- Upstream: the public Responses API (`api.openai.com/v1/responses`) with the bearer access token
  only, `store:false`, `stream:true`, `input` as an array; success only on `response.completed`.
  Plan-usage preview rules applied by the translator: system and developer turns go to
  `instructions` (no `system` items); `temperature`, `top_p`, `max_output_tokens`, `metadata` and
  the like are omitted; every agent tool is grouped in one namespace (`agent`), and calls carry it
  back, so the agent sees plain tool names. A forced specific tool becomes `tool_choice: required`.
- Models: after sign-in, `/v1/models` is the plan's catalog (`visibility: "list"`, server order,
  cached 10 min) with a context length when the catalog publishes one (else 272,000); before
  sign-in it serves the configured default (`gpt-6-luna`).
- Also: images, `reasoning_effort` (clamped to the nearest weaker supported level, never a 400),
  usage, and encrypted reasoning cached in memory by `call_id` and replayed on the next tool turn.
- Errors: plan problems arrive in the stream (`response.failed`), so core holds the HTTP status
  until the first output and maps them: `subscription_sharing_usage_limit_exceeded` → **429**
  (`usage_limit_reached`, `resets_in_seconds`, `Retry-After`; no reset time is published, so core
  holds the plan off for `SWARM_CORE_PLAN_HOLD_SECONDS`, default 900, and answers 429 at once
  meanwhile), `…_usage_unavailable` → **503**, `…_user_not_eligible` / `…_route_not_supported` →
  **403**, `…_invalid_user` → **401** (sign-in marked expired), `…_unsupported_capability` → **400**
  (a core translation bug). Not signed in or refresh rejected → **401**; upstream failure → **502**;
  timeouts → **504**; fleet queue full → **503**. A failure after output has started can only be a
  stream error event.
- Sign-in: `stormo core login` on the owner's Mac ("Continue with ChatGPT"): browser loopback on
  `127.0.0.1:1455/auth/callback` (`SWARM_CORE_LOGIN_PORT`; an OS-assigned port when busy) with PKCE,
  state and nonce. First sign-in registers with `client_id=dynamic_agent_client`,
  `agent_name_hint=<instance name>` and a stable `ext_agent_host_id` (`urn:uuid:…`); the issued
  `oaiapp_…` client id is saved before the code exchange, and later sign-ins reuse it. The ID token
  is verified against OpenAI's JWKS (signature, issuer, audience, nonce, subject). Tokens and the
  registration live in `.swarm/core/auth/` (0600, gitignored): local and under the owner's control,
  as the program's terms require. `stormo core logout` drops the tokens and keeps the registration.
- Refresh: only the server refreshes (`grant_type=refresh_token`, `client_id`, `resource`), one at a
  time under a lock shared with `stormo core login`; the rotated token is written atomically before
  use, the file is re-read under the lock, and a refreshed ID token must be the same account.
- Plan UI copy (OpenAI's guidelines): after sign-in "You're using your ChatGPT plan"; status shows
  "Using ChatGPT plan" with **Manage usage** → https://chatgpt.com/settings/usage, which is also the
  action when the limit is hit. The web UI carries the same notice, indicator and link.
- Never logged: tokens, prompt or completion bodies. Logged: agent, model, status, duration, tokens.
- Fleet limits: one plan (and its weekly per-app cap, set by the owner in ChatGPT) is shared by every
  agent: concurrency cap `SWARM_CORE_LLM_CONCURRENCY` (default 3) and per-agent usage counters.
- Scope: **local only.** A paid or remotely hosted app needs OpenAI's interest form; agents on AWS
  keep their own API keys and never call this. Agents keep OpenRouter as `fallback_providers[0]`.

## 2. Registry and monitoring

Core knows the fleet from `agents/*/agent.yaml` (via `pkg/manifest`) and builds each agent's state
from three sources:

| source | how | gives |
|---|---|---|
| container | `ops.status("local")` (compose ps) every 15 s | running / stopped / starting, health check |
| engine | `GET :<port>/health/detailed` with the agent's `API_SERVER_KEY` | `gateway_busy`, `active_agents`, platform states, readiness. **Built** (`pkg/core/fleet.go`): every 3 s for running local agents, `/health/detailed` + `/api/sessions?limit=5` + `/api/jobs` with the agent's `API_SERVER_KEY` (read in the core, never sent on; an agent whose manifest fails to load is retried every 30 s without costing the others their keys); only counts, platform states and the newest session's source and time reach `/api/fleet`, never titles or previews |
| activity | Hermes `hooks.outbound` → `POST /ingest/hermes` (HMAC-signed) | session start/end, `pre/post_tool_call` (tool name, short redacted preview), approvals pending. **Built** (`pkg/core/monitor.go`), see below |

Derived state per agent: `down`, `starting`, `idle`, `busy` (current tool and since when),
`waiting` (approval pending), `degraded` (health failing, LLM on fallback, nap sidecar down),
`stale` (no nap within 2× its interval). Transitions are events: stored, streamed to the UI and,
when they matter (down, degraded > 5 min, stale), raised as findings.

**Activity ingest (built).** For an agent on the core (`engine.local.via: core`), a local build adds
the core to its Hermes config as an outbound hook target (`hooks.outbound`, name `swarm-core`,
`pkg/engine/hermes`): `on_session_start|end`, `pre|post_tool_call`, `pre_approval_request`,
`post_approval_response`. Hermes posts each one from a background queue (best effort, never
blocking a turn), signed `X-Hermes-Signature-256: sha256=<HMAC-SHA256 of the body>` with the
agent's `SWARM_CORE_KEY` (`secret_env`); the core finds the agent by the key that verifies, refuses
unsigned, unknown or out-of-window deliveries (the timestamp is inside the signed body, ±10 min) and
ignores a repeated `delivery_id`. LLM call hooks are left out on purpose: their payloads carry the
whole conversation, and the gateway already knows which calls are in flight. Outbound targets need
no `hooks_auto_accept` (that gates shell hooks only). From the deliveries the core keeps, per
agent, the tool calls in flight (by `tool_call_id`; one whose `post_tool_call` never came stops
counting after 15 min), open sessions and approvals waiting, served as `live` on `/api/fleet`; the
office's "working" also holds while a tool runs, and its label names the tool. Each delivery is a
row in `core.db` `activity`: event, tool, session and a preview that is the file a tool names, the
first 160 characters of a terminal command, or the names (never values) of other tools' arguments,
run through the instance's PII and secret scrubber. Previews and sessions reach only
`/api/agents/<id>/activity` with the owner token (or that agent's own key) and
`stormo core activity <agent>`; the office's agent panel lists `/timeline` (event, tool, time).
Rows are pruned after 7 days.

"Ping" is the poll above, plus an optional deep ping: `stormo core ping <agent>` sends a short
turn through the agent's API (`/v1/runs`, session `core-ping`) and checks it answers. It costs a
model call, so it is on demand or daily, never every poll.

Retention: activity previews can hold client data, so they live only in `.swarm/core/core.db`,
are pruned after 7 days, and never reach git, S3 or Slack.

## 3. Web UI

Served by core at `http://127.0.0.1:18600/`, plain HTML + one JS module, no build step, live
updates over SSE (`/api/events`).

- **Fleet**: one card per agent with state, current tool or "idle since", model in use (plan or
  fallback), last nap, pending learnings and skill proposals, open findings, LLM usage today.
- **Agent**: live activity timeline, recent runs and sessions (from the agent's API), its inbox and
  outbox, locks it holds, findings. Actions: **nap now**, **learn** (nap + dream), **restart**
  (`ops.restart`, local), **move** (`ops.handoff` local ⇄ ECS: stop the source, sync naps,
  start the destination; the ECS side waits for the owner's confirmation in the UI), **chat**
  (one turn through its API).
- **Workdir**: file tree with last writer, last change and lock badges; file history.
- **Messages**: the inter-agent bus, by thread.
- **Learning**: per-agent proposals waiting for review with links to the `stormo learn` commands.
  Accepting stays a CLI and human step for now.
- **Findings**: compliance findings by severity, acknowledge or resolve.
- **Gateway**: login state, plan limit status and reset time, per-agent usage, concurrency queue.

Remote actions on AWS (later) are not offered in the UI until the owner decides otherwise.

**Built so far: the office.** `http://127.0.0.1:18600/` opens on a top-down office floor, game-style.
Each unit (`units/*/unit.yaml`) is an office off one of two hallways, with its own floor (wood, carpet
tiles, concrete or library carpet: `office.units` in `stormo.yaml`), windows in the
outer walls and something on the back wall. The middle of the floor is the **server room**, where the
core runs (a glass-walled room with a raised floor, the core rack, a fleet rack with one blade per
agent container, network and UPS racks, cooling), above the **lobby** with the main entrance, the
kitchen and a wall of world clocks (`office.clocks` in `stormo.yaml`, on the core's time). Each agent has its own desk and sits there as its portrait (`persona:` in the manifest,
newest `avatar/<slug>-avatar-vN.png`; `personas/<slug>` in the instance, or a sibling repo under
`SWARM_REPOS_DIR`; initials when there is none). The desk is dressed for the role from the persona's
`desk:` frontmatter (screen app, monitors, props, a floor item; catalog in `personas/README.md`), else
from its unit's defaults. Each agent is a small top-down person (SVG, front, back and side views)
coloured from the persona's `sprite:` block, with its portrait in its name tag. It types at the desk
while working; while idle it leans back, stretches, looks out of the window or walks to the kitchen,
the water cooler or the sofa, and hurries back when work arrives. Agents that start walk in from the
entrance, along the hallway, through their office door to their chair; agents that stop walk out.
The name tag says what the agent is doing ("Working · Slack", "Idle 12m · getting coffee"): that is
the signal, the body's position is flavour. **The office runs in the core** (`pkg/core/office.go`, a
1 s tick over the fleet snapshot and the gateway's calls in flight): it decides working (held 8 s
across tool gaps), when an idle agent gets up and where it goes, with fixed walk and dwell times, at
most two agents out in the lobby at once. `/api/fleet` carries each agent's `office` state (activity,
phase, start, end, slot, label, working, seq) and the server time `now`; every page plays the same
phases back by server time, so all viewers see the same thing, and a page opened mid-walk joins it
mid-walk. Paths are drawn for each viewer's layout and paced to end when the phase ends. The state is
in memory, not in `core.db`: a restarted core seats everyone again.

**The sync robot.** A small robot docks in the server room. Each agent's nap sidecar saves its home
to the store on its own; when the fleet poll sees a new nap (`latest.json` moved), the core diffs it
with the previous one and the robot rolls to that agent's desk, writes the note ("Atlas's nap ·
2 learning, 1 state"), then goes on to the next queued desk or back to its dock. Naps that land while
a visit is queued fold into it. Notes are counts per file class (learning: memory and skills;
state: databases; raw: conversation logs) and files removed, never paths or contents. The robot
shows the sync, it does not cause it; the first nap seen after a core restart is not visited (there
is nothing to compare it with). `/api/fleet` carries it as `robot` (phase, from, target, timings,
note, the last 8 notes, the queue); clicking it opens its notebook. Vacant desks fill each office. What the desk shows comes
from `/api/fleet`:

| desk | from |
|---|---|
| seated, screen lit (*at desk*); walking in from the door (*arriving*); grey ghost (*off shift*) | compose state, polled every 10 s |
| typing at the desk, "Working · <source>" (*working*) | the engine is running a turn (`activity.activeAgents`, `gatewayBusy`), a scheduled job is running (`activity.runningJobs`, e.g. a `no_agent` watchdog script, which runs no agent turn at all), or a model call is in flight; held 8 s across gaps; the label names the job when that is the only work, else the newest session's source (Slack, API run) |
| lilac bubble, packets from the rack (*thinking*) | an LLM call in flight through the gateway for that agent |
| screensaver, "Idle 12m", idle wandering (*idle*) | running, nothing running; time since the later of the newest session's last activity and the last scheduled job run |
| red ring and `!` (*needs attention*) | health check `unhealthy`, the nap sidecar not running, or a platform reporting `needs_attention` |
| gold marker with a count | learnings and skill proposals waiting for review |
| core rack lights and slots | sign-in state, plan limit, calls in flight vs the concurrency cap |
| the sync robot at a desk, with a note | a new nap of that agent landed in the store; counts of files changed per class |
| fleet rack blades | one per agent, lit by its state |

Clicking an agent, the rack or a vacant desk opens a panel with the details and the `swarm`
commands to copy. It is read-only for now: the actions listed above (nap, learn, restart, move,
chat) are still CLI steps. The page polls every 2.5 s rather than using SSE, and there is no
`core.db` yet; the activity feed is built in the browser from state changes. Per the plan-usage UI
guidance the core is labelled "Using ChatGPT plan", with a "Manage usage" link that becomes the main
action while the plan limit holds.

## 4. Shared workdir

`workdir/` at the repo root (gitignored), bind-mounted read-write into every agent container at
`/workdir` and read by core on the host. Every agent can read and write everything in it.

**Jail.** The jail is the mount: an agent container sees `/workdir` and nothing else of the host.
`HERMES_WRITE_SAFE_ROOT` (add `/workdir`) only stops Hermes' file tools; Hermes documents that the
terminal tool bypasses it, so it is a guard rail, not the boundary. Inside the jail nothing is
enforced at the filesystem level; attribution and locks are a protocol that core tracks and audits.

**Fit with the existing shared space.** CLAUDE.md requires that an agent only mounts
`/shared/group` and `/shared/<its unit>`. A free-for-all workdir is therefore a **group-scope**
space: it gets the same rules as `/shared/group` (no credentials, no client personal data), and
unit-private material stays in `/shared/<unit>`. Locally, `.swarm/shared/<layer>` moves under
`workdir/` (`workdir/group` = the free-for-all area, `workdir/<unit>` mounted only for that unit).
That keeps one tree for humans to browse and keeps the unit rule. (Open decision 2.)

**Standard access.** Agents use files normally (read, write, terminal) and go through core for
coordination:
- MCP tools (`mcp_servers.core`): `fs_lock`, `fs_unlock`, `fs_renew`, `fs_locks`, `fs_info`
  (last writer, history, lock), `fs_write` (writes through core: checked against locks, attributed
  exactly), `fs_note` (declare "I changed X" after a terminal write).
- A generated workdir skill with a stdlib Python helper (`workdir.py lock|unlock|info|ls`)
  calling the same HTTP API, for terminal use and scripts. It replaces the shared-docs skill locally
  once the workdir lands (the document conventions carry over).

**Attribution.** Last creator or modifier per file comes from, in order of precision:
1. `fs_write` through core (exact);
2. Hermes `post_tool_call` events for file tools (`write_file`, `patch`, …) carrying the path
   (exact, automatic);
3. a host-side watcher on `workdir/` (fs events + periodic hash scan) for everything else (terminal
   writes): the change is recorded; the writer is inferred from which agent had a terminal tool
   running at that moment, else `unknown`.

Stored in `core.db` (`files`, `file_events`) and mirrored read-only for plain `ls` users as
`workdir/.swarm/meta/<path>.json`, which agents may read but core alone writes.

**Locks.** Every lock has an owner agent, path or glob, reason, created and expiry time.

| kind | meaning | others | default TTL |
|---|---|---|---|
| `hard` | exclusive, I am writing this | `fs_write` refused; any other write is a compliance finding | 30 min, renewable |
| `soft` | advisory, I am working on it, coordinate first | allowed, but `fs_info`/`fs_write` warn and name the holder | 2 h |
| `temp` | short lease for one operation | as `hard` while held; never renewed, auto-expires | 5 min |

Locks expire on their own, are released when the owner goes `down` (after a short grace period),
and are visible as `workdir/.swarm/locks/<path>.lock.json` so a terminal `ls` shows them too.

## 5. Comms between agents

Slack stays the channel for people (and for @mention handoffs people should see). Core adds an
internal bus for agent-to-agent work that should not clutter Slack.

- MCP tools: `msg_send(to, subject, body, thread?, priority?, attach?)` where `to` is an agent,
  `unit:<id>` or `group`; `msg_inbox(unread_only)`, `msg_read(id)`, `msg_ack(id)`, `msg_thread(id)`.
  Attachments are workdir paths, never file contents.
- Delivery: stored in `core.db`; if the recipient is idle, core wakes it with a run on its API
  (`POST /v1/runs`, input `[AGENT MESSAGE from <sender>] <subject>, read it with msg_inbox`), the
  same pattern the addressability plugin uses for `[AUTOMATED ALERT]`. A busy recipient gets it at
  its next `msg_inbox`. Urgent messages also mirror to the recipient's Slack home channel.
- Loop guards: per-pair rate limit, hop counter and TTL on threads, no wake-up for acks, and a
  fleet-wide cap on wake runs per hour. Breaches become findings.
- Rules: cross-unit messages follow the group rules (no client personal data); the bus is not
  memory (lessons still go through naps and review).

Hermes' kanban board was considered and set aside: its dispatcher spawns workers as local
subprocesses of one Hermes install, which does not fit one container per agent.

## 6. Learning phase orchestration

Core triggers and sequences the existing nap/dream loop; it does not replace it.

- **Nap trigger**: locally via the same path as `stormo nap-now` (`compose exec nap …`). For AWS
  later, the nap sidecar grows a small authenticated trigger listener (`nap --loop --listen`),
  which also works locally.
- **Learning cycle** (per agent, on a schedule such as nightly, or from the UI): nap now → wait for
  the nap → dream into `learnings/` (working tree only, as today) → count new proposals → notify the
  owner (UI, and Slack through the core agent). Accept, reject and promote stay human
  (`stormo learn …`); the core agent may *recommend* decisions in its digest.
- **Quiet time**: no nap trigger while an agent is `busy` unless forced, and naps across the
  fleet are staggered.
- Core never commits; the dream already writes the working tree only.

## 7. Compliance validation

Rule-based checks in the service, LLM-judged checks in the core agent. Each produces a finding
`{agent, rule, severity, evidence, first_seen, last_seen, status}` in `core.db`.

Rule-based (service):
- **Drift**: the running baseline (nap `baseline.gitSha` and skill hashes) differs from what
  `stormo build` would produce now; the engine image tag differs from the manifest.
- **Runtime self-modification**: new or changed skills, cron jobs or plugins since boot that are not
  yet proposals; cron jobs that deliver outside the agent's home channel.
- **Secrets**: token patterns (the dream's PII scrubber patterns plus provider key prefixes) in
  `workdir/`, shared layers, messages or activity previews.
- **Client data in group scope**: the PII scrubber run over new files in `workdir/group` and over
  cross-unit messages.
- **Unit boundary**: a message or attachment path that reaches another unit's private layer.
- **Lock violations**: a write to a file under another agent's `hard`/`temp` lock.
- **Liveness**: down, degraded, stale nap, nap sidecar not running, health check failing.
- **Model routing**: an agent stuck on fallback for hours, a local agent calling a provider that is
  not its manifest's, gateway 429 storms.
- **Behaviour signals**: tool-call loops (same tool and arguments repeated), runs stuck in
  `waiting_for_approval`, unusual token burn per agent.

LLM-judged (core agent, sampled, local only): replies that break the agent's `SOUL.md` hard rules
(for example answering an `[AUTOMATED ALERT]` that was not actionable, or client-facing tone when
not flagged client-facing). Samples come from the agent's own API (`/api/sessions/{id}/messages`).
They contain client data, so they are judged locally and only the finding, with a session id and a
redacted excerpt, is kept.

Findings never trigger restarts. Severity `high` alerts the owner; everything shows in the UI.

## 8. The core agent

`agents/core/` in `units/group`, engine Hermes like the others.
- Tools: core's MCP server with the fleet scope (`fleet_status`, `findings_list|ack`,
  `nap_trigger`, `learn_run`, `learn_pending`, `msg_*`, `fs_*`), plus its own Slack app.
- Schedules (Hermes cron): hourly script-only (`no_agent`) health digest that stays silent when
  all is well; a daily fleet digest with learning proposals to review; a weekly compliance sample.
- SOUL: steward, not operator. It reports, explains and recommends; it never restarts anything in
  production, never accepts its own or others' learnings, and never reads unit-private layers.

## Data (core.db, SQLite, `.swarm/core/`)

`agents` (last state), `state_events`, `activity` (7-day retention), `llm_usage`, `messages`,
`deliveries`, `files`, `file_events`, `locks`, `findings`, `learn_runs`. `auth/chatgpt.json` stays a
separate 0600 file and is never in the database.

## HTTP surface

| path | who | purpose |
|---|---|---|
| `GET /health` | anyone | liveness, login state (no secrets) |
| `/v1/chat/completions`, `/v1/models` | agent key | LLM gateway |
| `POST /mcp` | agent key | MCP (streamable HTTP): comms, workdir, fleet tools by scope |
| `POST /ingest/hermes` (built) | HMAC per agent | Hermes outbound hook events |
| `/api/*`, `GET /api/events` (SSE) | loopback browser or agent key | UI and CLI JSON |
| `GET /api/fleet` (built) | loopback, no key yet | roster, cached compose state, review counts, gateway state, each agent's `live` tool; no secrets, no account email |
| `GET /api/agents/<id>/timeline` (built) | loopback | the agent's newest hook events: event, tool, time |
| `GET /api/agents/<id>/activity` (built) | owner token, or that agent's key | the same with sessions and previews |
| `GET /avatars/<agent>.png` (built) | loopback | the agent's persona portrait, read in place |
| `/`, `/ui/app.js`, `/ui/style.css` (built) | loopback browser | the office UI |

## Repo layout

```
pkg/core/
  server.go            net/http: routing, auth (agent keys), embedded UI (built)
  keys.go              key → agent map from secrets.local.yaml (reload on change) (built)
  db.go                SQLite schema and migrations (built)
  owner.go             the owner token for private reads (built)
  llm/chatgptauth.go   Sign in with ChatGPT: browser PKCE sign-in, ID-token check, single refresher (built)
  llm/translate.go     chat.completions ⇄ Responses API (request, SSE, errors, reasoning cache) (built)
  llm/gateway.go       /v1 routes, concurrency, usage, timeouts, error mapping (built)
  fleet.go             roster from manifests, async compose poller, cached snapshot, avatars (built)
  monitor.go           activity ingest, live state, timeline (built)
  workdir.go           watcher, attribution, locks
  bus.go               messages, delivery, loop guards
  learn.go             nap triggers, learning cycle
  compliance.go        rule checks → findings
  mcp.go               MCP server (tools above)
  ui/                  index.html, app.js, style.css: the office (built)
docs/core.md           this plan
```

## Handoffs to the agent side

These touch `pkg/engine/hermes`, `pkg/local`, `pkg/manifest` and `agents/*/agent.yaml`,
which another session owns right now:
1. Custom provider → `http://host.docker.internal:18600/v1`, model `gpt-6-luna`, key from
   `SWARM_CORE_KEY`; OpenRouter stays `fallback_providers[0]`; retire the per-agent `auth.json`
   mount and `stormo login` (in progress there).
2. A local-only secret notion so `SWARM_CORE_KEY` is never expected or pushed in AWS.
3. `mcp_servers.core: {url: http://host.docker.internal:18600/mcp, headers: {Authorization: "Bearer ${SWARM_CORE_KEY}"}}`.
4. ~~`hooks.outbound` to `/ingest/hermes`~~ **Done** (phase 3): session, tool and approval events,
   signed with the agent's `SWARM_CORE_KEY`; outbound targets need no `hooks_auto_accept`.
5. `/workdir` bind mount, `HERMES_WRITE_SAFE_ROOT` += `/workdir`, `SWARM_WORKDIR=/workdir`.
6. `extra_hosts: host.docker.internal:host-gateway` on the agent service (harmless on OrbStack,
   needed on Linux).
7. Drop `API_SERVER_ENABLED` from the engine env: Hermes 0.21.5 never reads it (a ≥16-char
   `API_SERVER_KEY` is what enables the API server).

## Phases

| # | deliverable | done when |
|---|---|---|
| 1 | LLM gateway + `stormo core up/down/serve/login/logout/status` | an agent streams a tool-calling turn through core; plan limit falls back to OpenRouter; tests cover translation, refresh single-flight, error mapping |
| 2 | server skeleton, `core.db`, registry, pollers, fleet page | UI shows every local agent's state live; stopping an agent flips its card within 30 s. **Built**: office UI, registry, compose poller, `core.db` (migrations by `user_version`) |
| 3 | activity ingest + agent page | UI shows the current tool of a busy agent in real time. **Built**: signed ingest, live tool per agent on `/api/fleet` and in the office, agent timeline, `stormo core activity`; the UI polls (2.5 s), no SSE yet |
| 4 | workdir mount, watcher, attribution, locks, `fs_*` MCP tools, workdir skill | two agents contend for a file: the second sees the lock and holder; a terminal write is attributed |
| 5 | comms bus (`msg_*`), wake delivery, loop guards | agent A asks agent B a question and gets an answer with no human in the loop, with no Slack noise |
| 6 | learning cycle, nap trigger, schedule, digest | nightly cycle produces proposals and a summary without manual steps |
| 7 | rule-based compliance + findings page | each rule has a test fixture that raises it |
| 8 | `core` Hermes agent (persona, Slack app, cron, MCP scope) | daily digest in Slack; compliance samples judged locally |
| 9 | AWS (later) | core as an ECS service, workdir on EFS as a group access point, Service Connect, gateway disabled |

## Open decisions

1. **Host process vs container for core.** Planned: host process (simplest lifecycle control, no
   Docker socket). Revisit if core should run on a shared machine.
2. **Workdir and the unit rule.** Planned: `workdir/group` free-for-all, `workdir/<unit>` private
   per unit, replacing `.swarm/shared` locally. Alternative: one flat free-for-all `workdir/`, which
   would mean amending the CLAUDE.md shared-documents rule.
3. **Who may trigger naps and learning cycles**: core on a schedule by default; whether the core
   agent may trigger them on its own or only suggest.
4. **Alert channel**: a `#swarm-ops` Slack channel through the core agent's app vs. a plain webhook
   from the service (works before the core agent exists).
5. **ChatGPT plan usage: eligibility and allocation.** The program is documented for open-source,
   locally hosted apps. Each instance's owner reads the Sign in with ChatGPT terms
   (openai.com/policies/sign-in-with-chatgpt-terms) and decides whether their use qualifies before
   the first `stormo core login`. It is still a better footing than the Codex-CLI login it replaced (own
   OAuth client, public API, tokens local). If it goes ahead: every local agent shares one plan and
   its weekly per-app cap, so decide which agents go through core first and which go straight to
   OpenRouter.
6. **Retention** for activity previews (7 days planned) and messages (90 days planned).
