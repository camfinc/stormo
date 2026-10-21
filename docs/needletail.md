# Needletail

Needletail is a Stormo engine written in Go. It runs agents in place of the Hermes image and lets agents write their own tools in Luau. This document is the design and the contract: the formats it defines are frozen at the first nap of a needletail agent (see §8).

## 1. Goal and non-goals

**Goal.** One static Go binary (`CGO_ENABLED=0`) that sits behind the existing `pkg/engine` contract (`Engine` + `Runtime`) and gives an agent everything it has today:
- the agent loop (an OpenAI chat-completions client with core and cloud fallback)
- built-in tools, SKILL.md skills and two hot-memory files
- a scheduler, a Slack Socket Mode gateway, the core MCP client and signed hooks
- the HTTP API the core polls

It adds one capability: **agents write sandboxed Luau tools** that live inside their skills, so the existing dream harvests them as proposals.

Agents move one at a time by setting `engine.kind: needletail`. Hermes agents are untouched.

**Design principles**
1. **Parity through shared formats.** Memory (`\n§\n`), `cron/jobs.json` and the session lineage semantics match today's, so nap, rehydrate and dream need no change.
2. **Keep the core neutral.** Nothing outside `pkg/engine/needletail` and the two lines in `pkg/engines` names the engine. Any core change is engine-neutral, ships as its own PR and keeps Hermes behaviour identical.
3. **Agent-written code is untrusted.** Luau gets capabilities, not ambient authority. The capabilities a script may use are granted outside anything the agent can write.

**Non-goals for the MVP**
- Changing the `Engine`/`Runtime` interface signatures.
- A new manifest `tools:` section.
- Telegram, pairing, slash commands, voice, plugins, kanban or a skill curator.
- Approvals for dangerous commands. Agents that rely on approvals stay on Hermes until M8.
- Sandboxing the bash `terminal` tool beyond what the container provides (see §7).
- A native or cgo Luau build.
- SQLite.

## 2. Where it lives

Everything is in the stormo module and generic to any organisation.

```
pkg/engine/needletail/        adapter: the only package the core sees
  needletail.go               New, Kind, Layout, Memory, Reasoning, Image, Command, Port,
                              HealthCheck, EngineOwnedSkills, LogNoise
  compile.go                  Compile, needletail.yaml rendering, model connection resolution
  env.go                      Env
  schedules.go                cron/jobs.json render / MergeSchedules / Schedules / ReadSchedules
  runtime.go                  NewRuntime, APIKeyName, ValidAPIKey, Activity, WakeRequest,
                              HookSignature, ParseHook
  conversations.go            Conversations over the needletail API
  bench.go                    BenchArgv, ParseBench
  testdata/                   golden home, classify.json, hook, jobs, bench fixtures
pkg/engines/engines.go        + "needletail": needletail.New / needletail.NewRuntime
cmd/needletail/main.go        in-container binary: serve | oneshot | healthcheck | luau check
internal/needletail/
  wire/                       JSON types shared by the adapter and the binary (one source of truth)
  config/  llm/  loop/  tools/  skills/  memory/  sessions/  sched/
  slack/  mcp/  hooks/  api/  sandbox/  secrets/
  luau/                       wazero host, caps, prelude
  luau/wasm/                  build.sh (pinned wasi-sdk + Luau tag/sha), shim.c, luau.wasm (go:embed)
deploy/needletail/Dockerfile
docs/needletail.md            this document
docs/schedules.md             schedule semantics, shared by both engines
```

The runtime lives under `internal/` so no other module can import it. The adapter imports only `internal/needletail/wire`.

**Binary subcommands**

| Subcommand | Purpose |
|---|---|
| `serve` | The default entrypoint. |
| `oneshot -q <prompt> --format stream-json` | Bench mode. |
| `healthcheck` | GETs `localhost:8642/health`, so the image needs no curl. |
| `luau check <file>` | Validates the header and compiles only. |

**Image** (`deploy/needletail/Dockerfile`)
- Multi-stage. The builder is `golang:1.26.2-alpine` pinned by its multi-arch index digest; it cross-compiles `CGO_ENABLED=0 go build ./cmd/needletail` on the build platform for amd64 and arm64. Its context is only `go.mod`, `go.sum`, `cmd/needletail` and `internal/needletail` (`Dockerfile.dockerignore`), so the build fetches no module besides `go.yaml.in/yaml/v3`.
- The final stage is `alpine:3.22` pinned by digest, with `bash`, `python3`, `git` and `ca-certificates`. Existing skill scripts are bash and python, so the `terminal` tool needs them.
- `USER 10000:10000` (`shared.PosixUID`), `AGENT_HOME=/opt/data`, a `HEALTHCHECK` running `needletail healthcheck`.
- `ENTRYPOINT ["/usr/local/bin/needletail"]`, `CMD ["serve"]`. The bench runs `<image> needletail oneshot …` (`BenchArgv`), so the binary drops a first argument equal to its own name.
- Built locally as `needletail:<tag>`; where it is published is open (§11). `Image()` builds `<repo>:<image_tag or v+version>` and returns an error for an empty tag or `latest`.

## 3. Engine contract

### Engine

| Method | Needletail value |
|---|---|
| `Kind` | `"needletail"` |
| `Layout` | See §6 |
| `Memory` | `engine.Delimited("\n§\n")`, the same contract as Hermes |
| `Reasoning` | `none, minimal, low, medium, high, xhigh`, plus any further Hermes levels the core gateway accepts, copied in a test so existing manifests stay valid |
| `Image` | Pinned tag; rejects empty or `latest` |
| `Compile` | `SOUL.md`, `needletail.yaml`, `skills/**` (Stormo form, `${SKILL_DIR}` left for the runtime to resolve), `scripts/**`, Knowledge files under `skills/`, seeded `memories/*.md`, `cron/jobs.json`. Using both `a.Schedules` and a legacy cron file is an error, as for Hermes. Unknown reasoning levels are rejected. |
| `Env` | Merges `a.Env`, then sets `AGENT_HOME`, `SWARM_AGENT`, `SWARM_UNIT`, `SWARM_SHARED_DIR`, `SWARM_CORE_URL` (on core), `NEEDLETAIL_API_PORT=8642`, each `state[].env`, and the unchanged `SLACK_*` channel env (`core/wake.go:74` reads `SLACK_HOME_CHANNEL`). |
| `Command` | `["serve"]` |
| `Port` | `8642` |
| `HealthCheck` | `["CMD","/usr/local/bin/needletail","healthcheck"]` |
| `EngineOwnedSkills` | Empty map (the image bundles no skills) |
| `MergeSchedules` / `Schedules` / `ReadSchedules` | Same `cron/jobs.json` shape as Hermes. The code is factored so both adapters share the jobs codec. |
| `LogNoise` | `nil` |
| `BenchArgv` / `ParseBench` | `needletail oneshot --format stream-json -q <p>`. Same JSONL lines as Hermes (`tool_use`, `text`, `result{text,exit_code,tokens.total,error}`). |

`FormatZero` and `SlackManifestArgv` are not implemented.

### Runtime

| Method | Needletail behaviour |
|---|---|
| `APIKeyName` | `API_SERVER_KEY`. The existing secret name is reused, so no new Secrets Manager entries are needed. |
| `ValidAPIKey` | At least 16 characters, so existing secrets stay valid. New keys are minted at 32 or more. |
| `Activity` | A single `GET /v1/activity`. The runtime reports `running_jobs` directly, so there is no memo. |
| `WakeRequest` | `POST /v1/runs {input, session_id}` with an `Idempotency-Key` header. |
| `Conversations` | See §4.8. |
| `HookSignature` | Header `X-Needletail-Signature: sha256=<hex>`. |
| `ParseHook` | `wire.Hook` v1 mapped 1:1, with no fallbacks. |

### Interface changes

**Required, small and engine-neutral.** Each is its own PR with Hermes parity.
1. `pkg/engines/engines.go`: two registry lines.
2. **Per-engine version validation.** `engine.version` and `image_tag` are validated today with Hermes-shaped rules. Move the check behind an optional `engine.VersionValidator` interface (`ValidVersion(string) error`). Hermes keeps its current rule; needletail accepts semver or a pinned tag. This lands before any manifest uses needletail.

**Made in M2, engine-neutral, Hermes unchanged.**
- The sidecar relay (`pkg/link`) forwarded only Hermes' hook headers, so another engine's signed hook reached the core unsigned (401). It now forwards any `X-<Engine>-Signature[-…]`, `-Event` or `-Delivery` header (each value at most 512 bytes, at most 8 of them, signatures first), besides its fixed set.
- The workdir's terminal inference (`shellAt`) read only Hermes' event names (`pre_tool_call`, `post_tool_call`). It also reads Stormo's kind names (`tool_start`, `tool_done`, `engine.HookKind`), which needletail's hooks carry; what the core stores is unchanged. The office timeline labels the kind names too.

**Deliberately not changed in the MVP.**
- `ShellTools` (`core/workdir.go:46`): needletail names its shell tool `terminal`.
- The `SLACK_HOME_CHANNEL` lookup in `wake.go:74`: needletail sets the same env var.
- The `mcp_core_*` tool naming: needletail registers core tools under the same names, so prompts and wake text behave the same.
- The `/ingest/<kind>` path.

**Later neutral follow-ups.** Each is a separate, behaviour-preserving PR and none is part of M0.
- `Runtime.ShellTools() []string`
- An `Engine.HomeChannel(a)` accessor
- A `stormo check` that fails if `Layout.RuntimeUID` differs from the shared-space owner
- Fix the "Hermes distribution" help text in `cmd/stormo/main.go`
- `docs/schedules.md` as the spec both engines implement (written in M4)

## 4. Runtime

The runtime is the process `needletail serve`, running as uid 10000 with `HOME=$AGENT_HOME`.

### 4.1 Configuration

`$AGENT_HOME/needletail.yaml` is rendered by Compile and read once at boot. It holds:
- `model`: `primary {base_url, key_env, model}` and `fallbacks[]`
- `reasoning`
- `limits {turns, command_timeout, script_timeout}`
- `core {mcp_url, ingest_url, key_env: SWARM_CORE_KEY}`, only when on core
- `channels`
- memory budgets
- `skills {generated}`: the skill dirs the build generates from the instance (the knowledge and shared-docs skills, from `CompileContext.Knowledge`), which `skill_manage` refuses to change (from M3)
- `luau {grants, http_secrets}`, taken from the agent's `engine/needletail/config.yaml`, which only the adapter reads

### 4.2 Secrets

- At boot the binary reads the values it needs (`SWARM_CORE_KEY`, `API_SERVER_KEY`, `SLACK_*`, the provider key) into a secrets struct.
- On Linux it marks itself non-dumpable (`prctl(PR_SET_DUMPABLE, 0)`), so `/proc/<pid>/environ` (the env it was started with, which unsetting does not change) and the rest of its `/proc` entry belong to root and a terminal command running as the same uid cannot read them. Commands it starts are dumpable again after their `execve`.
- It then unsets those secrets from its own environment except the terminal passthrough allowlist (`SWARM_CORE_URL`, `SWARM_CORE_KEY`, which helper scripts use to call the core as the agent) and the `http_secrets` envs, which are usually a skill's own token that its scripts read from the env. Skill secrets the runtime does not read stay in the env as with Hermes; every env value whose name looks like a credential (`*TOKEN*`, `*SECRET*`, `*_KEY`, …) is redacted.
- Tool results, logs and hook inputs go through a redactor that masks the known secret values, as they are and as JSON escapes them (with and without HTML escaping). Tool results are masked before they are encoded and again after; the terminal masks its output before cutting it to the cap, so a cut never leaves part of a secret.

### 4.3 LLM (`llm`)

An OpenAI chat-completions client with streaming SSE.

**Stream handling**
- Parses `content`, `reasoning_content`, `tool_calls` deltas keyed by `index` (a provider that sends none: a fragment with a new id starts a new call), the finish chunk, the usage chunk and `[DONE]`.
- An in-band `data: {"error":…}` frame, or a stream that ends without `[DONE]`, counts as a failed attempt.

**Retries and fallback**
- 429: honours `Retry-After` and `resets_in_seconds`.
- 503 `core_busy`: retried with backoff.
- Fallback chain: the core gateway first (local with `engine.local`). On a 401, a 429 `usage_limit_reached`, a connection error or a 5xx it moves to the cloud provider. The core is tried again on the next turn.
- On AWS the cloud provider is the only entry.

**Other**
- `reasoning_effort` comes from `limits.reasoning`.
- Tool call ids are returned verbatim, so the core can replay encrypted reasoning. A call that comes without one gets `call_<12 random hex>_<n>`, unique per response, so the core's call spans and live calls never mix two steps' calls.

### 4.4 Loop (`loop`)

A turn is `Run(ctx, session, input, opts{tools filter, model override})`.

**System prompt**, in order:
1. SOUL.md
2. the MEMORY block
3. the USER block
4. the skills index (name, description and dir per SKILL.md, read from the home every turn so a skill the agent just made is listed), pointing to `skill_view` and `skill_manage`. Only each SKILL.md's front matter is read (up to its closing `---`), and `node_modules`, `__pycache__` and `site-packages` directories are skipped. Names and descriptions are put on one line (whitespace and control characters collapsed), a description is cut to 1024 characters, a name to 64 and a dir to 128 in the list, and the whole section to 20 000 bytes, ending with `…and N more (skills_list)` when skills are left out: no skill can make every later turn outgrow the context
5. shared-space instructions
6. the current time

**Turn behaviour**
- The loop runs until the model makes no tool calls, `limits.turns` is reached, or the turn is cancelled.
- Tool calls run sequentially in the MVP.

**Compaction**
- Triggered when the token estimate passes 80% of `context_length` (from `/v1/models`).
- Older turns are summarised into a **new** session with `parent=<old id>`, and the old session ends with `end_reason=compression`.
- This matches today's lineage, `Tip` and `Compacted` semantics.

**Concurrency**
- One running turn per session and two globally.
- Over that cap the API returns 429 (`ErrBusy`). A run on a busy session returns 409.

### 4.5 Built-in tools

Names stay Hermes-compatible, so bench files, `schedules.tools`, `ShellTools` and the wake text keep working.

| Tool | Behaviour |
|---|---|
| `terminal` | `bash -lc` with cwd `$AGENT_HOME`, `command_timeout`, capped output, and an explicit environment: the process env without the runtime's own secrets, plus the passthrough allowlist. At the timeout (or when the turn is cancelled) the command's whole process group is killed. A process the command leaves running in the background after a normal exit (`cmd &`, say a server the agent starts on purpose) is **not** killed: once the command has exited, the tool waits 2 s for the output to close, then returns the output so far and the exit code with a note that background processes are still running; what they write later is not captured. When it runs from a skill it exports `SKILL_DIR` (the skill's absolute directory) and counts a use of it: the skill the call names (`skill`, a name or dir), else the one skill whose directory the command names (`skills/<dir>/…`, absolute or relative, with `skills/` at a path boundary, so `/tmp/otherskills/<dir>/` is not it), else, for a command that expands `SKILL_DIR` (`$SKILL_DIR` or `${SKILL_DIR}`; a command that only names it, such as `grep -r SKILL_DIR skills`, does not), the skill the session last viewed (kept in memory, for at most 256 sessions, and carried to the child a compaction continues the session in; a restart forgets it). A command that expands `SKILL_DIR` with none of these is refused rather than run with it empty. |
| `read_file`, `write_file`, `patch`, `search_files` | Paths are confined by `sandbox.Resolve` to `$AGENT_HOME` or `/shared/{group,<unit>}`, with symlinks evaluated and re-checked; a path must resolve inside the same root it names, so a symlink from one root into another (say `/shared/group/x` → the home's `skills/`) is refused. Only regular files are opened (a FIFO would block the turn), without following a symlink in the final component, and a write truncates only after checking that the path still resolves to the file it opened. `read_file` holds at most one capped line in memory; `patch` refuses files over 8 MiB. Direct writes to `/shared` are allowed for parity and followed by an automatic `fs_note` (decided, §11; §4.11); prompts prefer `fs_write`. The runtime's own files (`sessions/**`, `state/needletail/**`, `needletail.yaml`) are never written by these tools, and `search_files` skips them (`logs/**` too) and refuses to start in one. |
| `memory` | Actions add, replace and remove on the `memory` or `user` target. The budget is counted in UTF-16 units; when over, it returns an error showing the current usage. |
| `skills_list`, `skill_view`, `skill_manage` | Hermes' names and arguments. A skill is named by its dir under `skills/` (`crm/crm-api`), its front-matter name or its dir's last segment; an ambiguous name is refused with the candidates. `skills_list` lists name, description, dir and `read_only`. `skill_view {name, file_path?}` loads progressively: SKILL.md (or one file of the skill, at most 100 KB, binary refused) with `${SKILL_DIR}` resolved to the skill's absolute directory, plus the skill's other files (hidden files and nested skills left out). `skill_manage {action, name, …}`: `create` (name and optional one-segment `category`, both `[a-z0-9][a-z0-9_-]*`; `content` is the whole SKILL.md and must start with front matter naming the skill and describing it), `edit`, `patch` (`old_string` must match once unless `replace_all`; `file_path` omitted is SKILL.md), `write_file`, `remove_file` (empty directories it leaves are removed) and `delete` (one rename takes the skill out of `skills/` into `.needletail-tmp/`, then its files are removed, so a nap never sees half a skill). `delete` refuses a skill the instance booted with from the agent's repository (a dir in `.swarm/booted.json`'s `skills`, which rehydrate writes): rehydrate would bring it back at the next restart and the dream proposes no deletion, so removing it is a repo change a person makes; the agent may still change it. It refuses the build's generated skills (`skills.generated`, by dir or by name anywhere, since the dream skips them by name), hidden dirs, symlinks anywhere below the home, `..` and absolute paths, control characters in a path, a file named `SKILL.md` in any case other than the skill's own (`write_file` cannot make a nested skill that skips `create`'s checks), a file of a nested skill, a rename (the front-matter name must stay), a description over 1024 characters (unless it is the one the SKILL.md already had), files over 1 MiB, and a secret value the runtime knows anywhere in the call (`name`, `category`, `file_path` and the texts) or in the file a write would leave (the library's `Check` sees the whole patched file, so a secret split across two patches is refused too). Since a view shows `${SKILL_DIR}` resolved, what `create`, `edit`, `patch` and `write_file` write has the skill's absolute directory (followed by `/`, a quote, a space or the end, never a longer name that starts like it) put back as `${SKILL_DIR}`, and `old_string` is matched in that form first, then as given (a file that holds the literal path). A script therefore finds its directory in the `SKILL_DIR` env var (bash `$SKILL_DIR`; Python `os.environ["SKILL_DIR"]`), which the terminal exports. Repo skills may be changed (the dream proposes them as changed). Writes are atomic (`fsx.WriteAtomic`), files 0644 and directories 0755 as rehydrate makes them. `.luau` files are refused until M6, which adds the header check and compile before saving. `read_file` of a SKILL.md counts as a view. |
| `luau_test` | Dry-runs a Luau tool with writes and http disabled, so the agent can iterate. |
| `send_message` | Posts to the Slack home channel or a thread. |
| `cronjob_manage` | Hermes' scheduling tool (its name, toolset `cronjob`, and its arguments where they mean something here): `list`, `create`, `update`, `pause`, `resume`, `remove` and `run` (due now) on the agent's jobs and the repo's. One-time schedules are refused (agent.yaml cannot hold them). Never offered to a scheduled run. See docs/schedules.md §7. |
| `mcp_core_<name>` | The core MCP tools. |
| `<skill>__<tool>` | Valid Luau tools (§5). |

Skill usage counts go to `skills/.usage.json`, an object keyed by skill name in Hermes' keys, so the file (which the dream copies to the agent's `learnings/skill-usage.json`) reads the same for agents on either engine, and one an agent brings from Hermes keeps counting (keys needletail does not maintain, such as Hermes' curator fields, are kept):

| Key | Meaning |
|---|---|
| `use_count`, `last_used_at` | a SKILL.md view, a file of the skill viewed, or a terminal command run from it |
| `view_count`, `last_viewed_at` | its SKILL.md viewed (`skill_view` or `read_file`) |
| `patch_count`, `last_patched_at` | changed with `skill_manage` (refused changes are not counted) |
| `created_at`, `created_by` | first counted, or created by the agent (`"agent"`; otherwise `null`) |
| `state` | `"active"` |

Times are RFC 3339 with microseconds and `+00:00`, as Hermes writes them. `delete` drops the skill's entry. A count that cannot be written is lost; it never fails the tool call. Entries are keyed by name, as in Hermes, not by dir: `create` refuses a name another skill has, but two repo skills with the same front-matter name (in different dirs) share one entry, and a skill deleted and created again under its name starts afresh.

### 4.6 Skills and memory

- Skills are read from `skills/` in Stormo form, with no rendering. Needletail resolves `${SKILL_DIR}` itself (`skill_view`, and the terminal's `SKILL_DIR`), and `skill_manage` puts it back when it writes, so skills stay in Stormo form on disk and in the dream's proposals.
- The generated knowledge and shared skills arrive through `CompileContext.Knowledge`; Compile lists their dirs in `needletail.yaml` (`skills.generated`), and the runtime shows them like any skill but never changes them.
- What the agent writes under `skills/[!.]*/**` is Learning: the nap keeps it, rehydrate brings an agent-created skill back always and a changed repo skill back while the repo has not moved under it, and the dream proposes both (new or changed) with no change to `pkg/loop`. `skills/.usage.json` comes back through rehydrate's `skills/.*` rule and is copied by the dream.
- What keeps the agent's skills from harming its own turns or the review is in §4.4 (the bounded skills index in the system prompt) and §4.5 (`skill_manage`'s refusals: descriptions over 1024 characters, a nested `SKILL.md`, control characters in paths, secrets anywhere in the call or the file it leaves; `delete` of a repo skill, which rehydrate would restore). `serve` empties `.needletail-tmp/` at boot (§6).
- Memory is `memories/MEMORY.md` and `memories/USER.md`, entries separated by `\n§\n`, with no trailing newline. The seed is merged by the existing rehydrate code. Entries are trimmed of the same characters the dream and rehydrate trim (every Unicode space and the BOM; `strings.TrimSpace` would differ on U+0085 and U+FEFF), and both budgets count UTF-16 units of the whole file, so the memory tool, the merge at boot and the ledger see the same entries within the same budget (`TestMemoryParity`).
- Conversation lineage (a compaction ends a session and continues it in a child, §6) is Raw: it comes back verbatim at rehydrate, and the dream never reads it.

### 4.7 Schedules (`sched`)

`docs/schedules.md` is normative: it writes down Hermes' semantics (read from the image's `cron/` package) and lists where needletail differs. As built in M4:

- **File.** `cron/jobs.json` in the `{"jobs":[…]}` shape the adapter renders and Hermes writes, read and written as ordered raw objects, so keys the runtime does not model survive and an unchanged file is written back byte for byte. Needletail adds one state key, `running`, and keeps Hermes' `monitor_state` and `failure_streak`. The scheduler is the file's single writer: its tick, its runs and the `cronjob_manage` tool change it under one lock, re-reading it first, and write it through `fsx.WriteAtomicIn` and `.needletail-tmp/`.
- **Timing.** `interval` (minutes after the last run) and 5-field `cron`, parsed in-house (`sched.ParseCron`: lists, ranges, steps, names, Sunday as 7, Vixie's day-of-month/day-of-week OR) on the container's wall clock (`TZ`, else UTC; the binary embeds the zone database). The tick runs every 30 s; a due job runs once however many of its times passed; `next_run_at` moves before the run starts and `running` is set, so a crash never runs it twice, and boot records a run a restart cut short as failed.
- **Turns.** An agent job reserves a turn slot (`api.Server.Reserve`, the API's per-conversation and global caps of two) before its monitor or script runs; a busy agent leaves it due for the next tick. The run is a turn in its own session `cron-<id>-<UTC time>` with source `cron` (the hooks' platform), created only when the turn runs and ended (`cron_complete`) after it. Script-only jobs take no slot.
- **Run.** Monitor first (exact-output hash in `monitor_state`, snapshot in `state/needletail/monitor/<id>.out`; unchanged is `no_change`; the baseline or the unified diff and current output go into the prompt; state stored before the turn), then the pre-run script (its output, or its error, in the prompt; empty output or `{"wakeAgent": false}` is silent), then the prompt: each skill loaded with `skills.Library.View` for the run's session (a counted view, the session's current skill for `SKILL_DIR`), a short cron hint, the script block, the job's prompt and the monitor block. `enabled_toolsets` expand Hermes' toolset names (`skills` → skills_list, skill_view, skill_manage; `file`; `terminal`; `memory`; a tool's own name) before `loop.Options.Tools` filters, the core's `mcp_core_*` tools stay unless `no_mcp`, and `cronjob_manage` and `send_message` are never offered to a run. `model`/`provider` are not applied yet (logged).
- **Outcome.** `ok`, `silent` (`[SILENT]` as the reply or its first or last line), `no_change`, `error` (a failed turn, `[CRON_FAILURE]`, an empty reply, a failing monitor or script-only script, an unrunnable job, which is also paused) and `delivery_failed`. The failure streak counts errors and any success resets it; from the third a notice says the job is worth a review. `repeat.times` completes the job. Every run that did something is recorded in `state/needletail/jobs/<id>/<time>.md` (the newest 10).
- **Delivery.** `deliver` and `failure_deliver` are parsed into targets (`local`, `origin`, `slack`/`home`/`all`, `slack:<channel>[:<thread>]`) and handed to a `sched.Deliverer`. Until M5 that is `sched.Pending`: a Slack target is accepted and logged (the output stays in the run's record, and `last_delivery_error` stays empty so review does not flag every job), another platform is a delivery error.
- **Scripts** run from the home's `scripts/` (symlinks resolved and confined), `.sh`/`.bash` with bash and anything else with python3 (`.luau` from M6), cwd the script's directory as in Hermes, the terminal's environment, a 1 h bound and a process-group kill.
- `GET /v1/activity` reports `running_jobs` and `last_job_at` from the scheduler.

### 4.8 API (`api`, port 8642)

All routes except `/health` need `Authorization: Bearer API_SERVER_KEY`.

| Route | Behaviour |
|---|---|
| `GET /health` | `{"status":"ok"}` |
| `GET /v1/activity` | `{active_turns, busy, source, running_jobs[], last_job_at, last_active, platforms{slack:{state,needs_attention}}}`. Counts, states, times and the running jobs' names (as Hermes' activity gives them; the adapter cuts each to 60 characters), no titles or previews. |
| `POST /v1/runs {input, session_id}` | Asynchronous, returns 202. The `Idempotency-Key` is kept for 24 h in `state/needletail/idem.json`, and a duplicate returns the original 202. |
| `GET /v1/sessions?limit` | Lists sessions. |
| `POST /v1/sessions {id}` | Creates a session; 409 if it exists. |
| `GET /v1/sessions/{id}/messages?limit&order=latest` | Transcript. |
| `POST /v1/sessions/{id}/chat {message}` | Synchronous; returns `{session_id: tip, message:{content}}`. |
| `DELETE /v1/sessions/{id}` | Deletes the whole lineage on the server. |

Once shutdown has begun (SIGTERM) no turn starts: `/chat` and `/v1/runs` answer 503. Running turns get the drain budget, then are cancelled and waited for at most a few seconds more, so a turn stuck in something that ignores cancellation cannot keep the process from exiting.

Status mapping on the client side: 404 → `ErrNotFound`; 409, 429 and 503 → `ErrBusy`.

### 4.9 Channels (`slack`)

A Socket Mode client:
- Calls `apps.connections.open` with `SLACK_APP_TOKEN` and acks envelopes. Reconnects with backoff.
- Handles `events_api` `message` and `app_mention` events.
- Users are allowed by `SLACK_ALLOWED_USERS` or `*_ALLOW_ALL_USERS`. Bot messages follow `SLACK_ALLOW_BOTS` (none, mentions or all).
- Each thread maps to a session `slack-<channel>-<thread_ts>`.
- Replies use `chat.postMessage` with `SLACK_BOT_TOKEN`. `SLACK_HOME_CHANNEL` is the delivery target.

The websocket client is `github.com/gorilla/websocket` v1.5.3 (decided, §11), added in M5.

### 4.10 Core MCP (`mcp`)

- Stateless JSON-RPC over POST to `core.mcp_url` (the sidecar relay's `/mcp`) with `Authorization: Bearer SWARM_CORE_KEY`.
- `initialize` with protocol `2025-06-18`, then `notifications/initialized`. `tools/list` is cached per boot once it succeeds; `tools/call` runs a tool.
- The core's tools join the registry as `mcp_core_<name>` as soon as the core lists them. The core (or the relay) may be away at boot: `serve` keeps asking with backoff (2 s doubling to 1 min), and turns before that run without them.
- A result with `isError`, the relay's "core unreachable" answer (JSON-RPC `-32000`) and a transport error all reach the model as a tool error; none fails the turn. A `msg_send` the relay queues while the core is away answers as a normal result ("Queued: …").
- An object result reaches the model as that JSON; any other text as `{"result": text}`.
- `serve` only: `oneshot` (the bench) never talks to the core.

### 4.11 Hooks to the core (`hooks`)

Each event is a `POST {ingest_url}/needletail` with header `X-Needletail-Signature: sha256=<hex HMAC-SHA256(body, SWARM_CORE_KEY)>`.

`wire.Hook` v1 body:
```json
{"v":1,"event":"session_start|session_end|tool_start|tool_done|approval_waiting|approval_answered",
 "delivery":"<uuidv7>","at":"<RFC3339Nano>","session":"","tool":"","input":{},
 "call_id":"<real tool call id>","approval_id":"","platform":"","status":"","duration_ms":0}
```

- Sent through a bounded queue (256 events, the oldest dropped when full) by one worker, in order, with a 5 s timeout per attempt, and only when on core (`serve`). Emitting never blocks a turn.
- A transport error or a 5xx is retried with backoff (1 s doubling to 30 s) with the same body and delivery id (a UUIDv7), so the core drops a repeat; a 4xx is dropped (the core refuses those for good; the first refusal and every 100th are logged); an event older than 9 min is dropped (the core refuses anything more than 10 min off). The relay answers 202 when it holds a delivery for a core that is away: that counts as delivered, and the relay sends it on later with its original `at`.
- Events: `session_start` when a turn begins and `session_end` when it ends (`status` ok, error or cancelled), `tool_start` and `tool_done` around each tool call (`call_id` the model's own id, `status` ok or error, `duration_ms`). `session` is the conversation's root, so a compaction mid-turn does not split start from end; `platform` is the session's source (`api_server`, `slack`, `cron`, …). Approval events wait for M8.
- No events are sent for LLM calls, and none for the assistant's text (client data).
- `input` is the call's arguments, redacted, each string cut to 512 bytes, objects to 32 keys (the path and command keys first), arrays to 16 items, 4 levels deep; an input still over 32 KiB keeps only its path and command keys. The relay accepts at most 1 MiB.
- After a file tool writes under `/shared`, an `fs_note` for its `/shared/<layer>/…` path is sent in the background (64 queued, retried while the core is away for up to 10 min; an attempt the core does not answer within 60 s counts as away). The `tool_done` hook names the path too, so the core's workdir attributes the write either way; a terminal write is attributed by the core's terminal inference.
- On SIGTERM the hooks and notes outlive the drain of running turns, and get up to 3 s more to go.

## 5. Luau

### 5.1 Library choice

**Luau 0.740** (released 2026-09-25, MIT), compiled to `wasm32-wasi` with a pinned wasi-sdk and run by **`github.com/tetratelabs/wazero` v1.12.0** (released 2026-05-28, Apache-2.0, no dependencies). wazero is the only new Go module in the whole design.

Why this choice:
- No cgo, so builds stay static and cross-compile for amd64 and arm64.
- Two sandbox layers: wasm memory isolation, and Luau's own sandbox inside it.
- The full Luau language, including type annotations.

Rejected:
- gopher-lua and go-lua: Lua 5.1/5.2, which cannot parse Luau syntax.
- gluau: needs cgo plus the Rust toolchain.
- lei: has no licence.
- aarzilli/golua: not Luau.

Fallback, only if profiling shows CPU-bound use: a cgo wrapper over the vendored C API behind the same `luau.Runner` interface.

**The wasm artifact.** `internal/needletail/luau/wasm/build.sh` pins the wasi-sdk version and the Luau tag and sha. `luau.wasm` is committed and embedded with `go:embed`, and its sha256 is pinned in a test. A spike before M6 settles error handling under WASI: `LUA_USE_LONGJMP` or wasm exception handling.

### 5.2 Sandbox

**Host side**
- One `wazero.Runtime` per process, configured with `WithMemoryLimitPages(1024)` (64 MiB) and `WithCloseOnContextDone(true)`.
- The module is compiled once. Each run gets a **fresh instance** under `context.WithTimeout(script_timeout)`, default 30 s.
- WASI has no preopens, no env, no args and a monotonic clock only. stdout and stderr are each capped at 64 KiB.

**C shim** (`shim.c`)
- `luaL_openlibs` is limited to base, string, table, math, utf8, bit32 and buffer.
- Removed: `loadstring`, `load`, `getfenv`, `setfenv`, `debug`, `require`, `newproxy`, `io` and `os`, except `os.time` and `os.clock`. `collectgarbage` accepts only `"count"`.
- `luaL_sandbox` is applied, then `luaL_sandboxthread` per run.
- An interrupt callback checks an instruction budget and a host deadline flag.
- A counting `lua_Alloc` enforces a soft cap of 48 MiB, which raises a catchable out-of-memory error.
- `LUAI_MAXCCALLS` is 200.
- Only source is accepted, compiled in the VM with `luau_compile` at O1. Agent-supplied bytecode is rejected.

**Host ABI.** A single import, `env.host_call(name_ptr,name_len,in_ptr,in_len) -> handle`, plus `host_result(handle,out_ptr,cap)`. Both directions are JSON:
- success: `{"ok":true,"v":…}`
- failure: `{"ok":false,"err":{"code","msg"}}`

Errors are raised as Luau errors on the C side, so nothing unwinds through Go.

### 5.3 Bindings: the frozen global `nt`

| Binding | Rules |
|---|---|
| `nt.fs.read/write/append/list/stat/remove` | Paths are lexically cleaned, then every component is checked with `Lstat` and symlinks are refused. Paths must be under `$AGENT_HOME`. **Protected paths** cannot be written: `needletail.yaml`, `SOUL.md`, `sessions/**`, `logs/**`, `state/needletail/**`, `memories/**` (memory changes only through the memory tool). `/shared/group` and `/shared/<unit>` paths go to the core's `fs_ls`, `fs_info` and `fs_write` (with `fs_lock` and `fs_note` semantics); direct mount writes are never made from Luau. Quotas per run: 8 MiB read, 4 MiB written. |
| `nt.http.request{method,url,headers,body,timeout_ms}` | HTTPS only. The host must match the effective grant (exact or `*.suffix`). A custom `DialContext` resolves DNS once and dials the pinned IP, rejecting loopback, RFC1918, link-local, `169.254.169.254`, ULA, and the core and Docker host addresses (the core is reachable only through `nt.core`). Proxy env is ignored. At most 3 redirects, each re-checked. Response cap 5 MiB, call cap 20 s. |
| `nt.core.call(tool, args)` | Calls the core MCP `msg_*` and `fs_*` tools as the agent, within the granted cap. |
| `nt.memory.read()` | Read-only. |
| `nt.json.encode/decode`, `nt.log(level,msg)`, `nt.now()`, `nt.args` | Utilities. |
| **No** `nt.env` and **no** `nt.secret` | Secrets reach HTTP only through `http_secrets` grants: host → env name → header template, injected by Go. Luau never sees the value, and it is redacted from results and logs. |
| **No** exec | There is no process capability. |

**Audit**
- Every host call is appended to `logs/audit/<session>.jsonl` (Raw) as `{ts, tool, cap, args_sha256, bytes_in, bytes_out, status}`.
- The outer tool call emits `tool_start` and `tool_done` hooks named `luau:<tool>`, with path keys so the core attributes shared writes.

### 5.4 Tool format (contract v1)

A tool lives at `skills/<cat>/<skill>/tools/<name>.luau`. It starts with a YAML header and its body returns a function:

```lua
--[[needletail
format: 1
name: lookup_fare
description: ...
args: {type: object, properties: {...}, required: [...]}
caps: [http:api.example.com, core:msg_send, fs.read:home]
timeout: 20s
]]
return function(args) ... end
```

**Capability grammar**

| Capability | Meaning |
|---|---|
| `fs.read:home`, `fs.write:home[/glob]` | Home access. |
| `fs.read:shared`, `fs.write:shared` | Shared-space access. |
| `http:<host>` | Outbound HTTPS to that host. |
| `core:<tool>` or `core:msg_*` | Core MCP tools. |

**Effective caps = declared ∩ granted.**
- Grants live only in `engine/needletail/config.yaml` (rendered into `needletail.yaml`). Both files sit outside `skills/` and are not writable by the agent, so a tool, or a dream-proposed tool, cannot grant itself anything.
- Default grants: `fs.read:home`, `fs.write:home` (excluding protected paths) and `core:msg_*` when on core.
- `http` and `fs.*:shared` are off unless granted.
- A call outside the effective caps fails with code `denied` and is audited.

**Lifecycle**
- `skill_manage` validates the header and runs `luau check` before saving.
- At turn start, valid tools are registered as `<skill>__<name>`. Invalid ones are listed with their error in `skill_view` and never registered.
- Because tools live under `skills/[!.]*/**` (Learning), the dream's existing skill diff proposes them with no dream change. The proposal review should show the declared caps.

## 6. Storage and mounts

**Home.** `Layout.Home = /opt/data`, the same path as `AGENT_HOME`, on the task-local volume. One process writes it, as uid/gid 10000.

**Snapshot rules** (first match wins; golden-tested in `testdata/classify.json`)

| Class | Paths |
|---|---|
| Learning | `memories/MEMORY.md`, `memories/USER.md`, `skills/.usage.json`, `skills/[!.]*/**` (including `tools/*.luau`), `cron/jobs.json`, `needletail.yaml` (napped but rebuilt from the baseline on every boot, as Hermes' `config.yaml`: a changed manifest or grant reaches the agent, and nothing the agent writes into it survives a restart) |
| Raw (PII) | `sessions/**`, `logs/audit/**`, `state/needletail/jobs/**` (schedule run records) and `state/needletail/monitor/**` (monitor outputs): what jobs fetched and replied is client data |
| State | `<StateRoot>/**`, `state/needletail/**` (idempotency keys, Slack state) |

`SOUL.md` and `scripts/**` are rebuilt from the baseline. `.needletail-tmp/` is claimed by no rule, and `serve` empties it at boot before anything writes through it (`oneshot`, which may run beside a serving runtime, does not): `skill_manage` and the usage counts write their temporary files there before renaming them into `skills/`, and so do the session header and `sessions/index.json` rewrites, `state/needletail/idem.json` and the scheduler's files (`cron/jobs.json`, monitor snapshots, run outputs) (a temporary file beside a skill file would be claimed by `skills/[!.]*/**`, so a nap taken mid-write, or a crash before the rename, would keep it as part of the skill). A deleted skill is moved there in one rename before its files are removed, so a nap never keeps half of it.

**Sessions.** One file per session, `sessions/<id>.jsonl`.
- Header line: `{v:1,id,parent,end_reason,created,source,title}`.
- Message lines: `{role,content,tool_calls,tool_call_id,at}`.
- Message lines are only appended, and fsynced at turn boundaries; a torn last line (a crash mid-append) is dropped at boot. The header is rewritten once, when the session ends (`end_reason`), through a temporary file, fsync and rename. `sessions/index.json` (every conversation's summary, for readers outside the runtime) is replaced by atomic rename; the runtime rebuilds its own index from the files at boot. This keeps the home crash-consistent for the nap's hashing, with no SQLite and no `SQLite:true` rule.
- A compaction ends the session (`end_reason: compression`) and continues in a new session (a fresh id) whose `parent` is the old one. The new file starts with one `system` row holding the summary, then the recent rows it carries over, copied verbatim with their `at`, so a session file alone resumes the conversation. The cut is always at a row before which every tool call has its result.
- The API follows the lineage: a conversation is listed as its tip with `root` set, `/chat` on any of its sessions runs on the tip, a transcript runs from the root and leaves out the summary rows and the carried copies, and `DELETE` removes every session of it. Tool rows carry `tool_name` in transcripts only; files pair them with their call by id.
- `modernc.org/sqlite` is already in `go.mod` and remains an option for an index if JSONL scanning becomes slow. It would add no module.

**Layout fields**

| Field | Value |
|---|---|
| `Memory` | `memories/MEMORY.md`, `memories/USER.md` |
| `SkillsDir` | `skills` |
| `SkillUsage` | `skills/.usage.json` |
| `Managed` | `[skills, scripts]` |
| `NotRestored`, `Placeholders` | none |
| `Schedules` | `cron/jobs.json` |
| `ScheduleSource` | `""` |
| `StateRoot` | `inst.Names.StateDir` |
| `RuntimeUID/GID` | `shared.PosixUID` |

**Mounts**
- `/shared/group` and `/shared/<unit>` are mounted read-write exactly as today, because the bash terminal needs them.
- File tools are confined by `sandbox`; Luau writes to `/shared` go through the core.
- The agent's private `data/` is never mounted.
- Secrets arrive only as container env.

## 7. Security

- **The real boundary is the container.** Its uid is 10000, it runs with `no-new-privileges` and dropped capabilities, and its only mounts are home and its own shared directories. The bash `terminal` tool has the same reach as in Hermes today. The Luau sandbox constrains only Luau.
- **Luau:** two layers (wasm and the Luau sandbox), capabilities granted out of band, no env, no secrets, no exec, protected paths, symlink refusal, quotas, an SSRF dialer that pins the IP, and an audit log.
- **Secrets:** read into a struct and unset from the env except the passthrough allowlist; the process is non-dumpable so its start-up env in `/proc` is not readable by the terminal; the terminal gets an explicit env without them; redacted from outputs, logs and hooks; injected into HTTP headers only by Go.
- **API:** bearer key on everything except `/health`. Idempotent runs and a busy cap.
- **Hooks:** HMAC-signed and timestamped (the core rejects anything more than ±10 min off). Unique delivery ids.
- **Supply chain:** one new module (wazero v1.12.0, at least 2 weeks old). Base images pinned by digest. The `luau.wasm` sha256 is pinned and its build is reproducible from `build.sh`.
- **Adversarial test suite (M6)**, which must fail closed:
  - infinite loop, killed at the deadline
  - memory bomb, which gets an out-of-memory error
  - `../` and symlink escape
  - writes to protected paths
  - SSRF to `169.254.169.254`, RFC1918 and the core
  - redirects to a disallowed host
  - an undeclared or ungranted cap
  - no env visible
  - agent-supplied bytecode refused

## 8. Versioned contracts

Each is frozen at the first nap of a needletail agent and covered by golden tests in `pkg/engine/needletail/testdata`:
- the memory separator `\n§\n`
- the `skills/.usage.json` shape (Hermes' keys, §4.5)
- the `cron/jobs.json` shape (shared with Hermes)
- session JSONL v1
- hook body v1 and its signature header
- the Luau tool header v1 and the capability grammar
- the `host_call` ABI
- the snapshot rule list

## 9. Milestones

Every milestone ships as a draft PR behind `engine.kind: needletail`.

| # | Milestone | Done when |
|---|---|---|
| M0 | **Adapter skeleton**: `pkg/engine/needletail`, `internal/needletail/wire`, registry lines, Layout and rules, Compile, Env, schedules codec, Runtime and Conversations clients, bench parser, this doc. Preceded by the neutral version-validator PR. | `gofmt`, `go vet` and `go test ./...` are green. `stormo build` of the examples/minimal atlas agent with `engine.kind: needletail` matches the golden home. Every emitted and runtime path is classified. Hermes goldens are unchanged. |
| M1 | **Runtime core**: `serve`, `oneshot` and `healthcheck`; llm with fallback; loop with compaction; sessions JSONL; API; `terminal`, file and memory tools; secrets scrub and redactor; Dockerfile. | Locally on the core, the agent answers `stormo chat`. The bench passes on the atlas bench files. Activity and wake work against a real core. |
| M2 | **Core integration**: MCP client (`mcp_core_*`), signed hooks, ParseHook, automatic `fs_note`. | The office shows live tool calls with real call ids. A `msg_send` between a Hermes agent and a needletail agent round-trips with a wake. Shared-file attribution credits the needletail agent. |
| M3 | **Skills and memory parity**: index, `skill_view`, `skill_manage`, `${SKILL_DIR}`, usage counts, lineage compaction. | A nap, rehydrate and dream cycle on a needletail home yields ledger sightings and a skill proposal from an agent-created skill, with loop code unchanged. Proven by `TestNapRehydrateDream` (`internal/needletail/cli`): the real build, rehydrate, oneshot turn, nap and dream, then a second boot from the nap. |
| M4 | **Scheduler** plus `docs/schedules.md`. | Table tests built from real agent job definitions pass. `stormo review` shows schedule status. An agent-made job survives a rehydrate. Proven by `TestRealJobs` and `TestReviewReadsTheRuntimesFile` (`internal/needletail/sched`: anonymised real job definitions rendered by the real build, run through every outcome; review's `AnalyzeSchedules` over the file the runtime writes), `TestAgentJobSurvivesRehydrate` and `TestServeRunsADueJob` (`internal/needletail/cli`), and a `stormo review` of a running needletail agent on a temporary copy of the example instance. |
| M5 | **Slack gateway**. | A test Slack app talks to a needletail agent in DMs and threads, and schedules deliver to the home channel. |
| M6 | **Luau tools**: build script, embedded wasm, wazero host, `nt.*`, the grants file, `luau check`, `luau_test`, `.luau` schedule scripts and monitors. Preceded by the longjmp/EH spike. | An agent writes a tool through `skill_manage`, calls it next turn, and the dream proposes it. The adversarial suite passes. The image builds `CGO_ENABLED=0` for amd64 and arm64. |
| M7 | **First cutover**: one low-risk agent switches `engine.kind` with a pinned `image_tag`; ECS task definition render; soak. | One week with no regression against its Hermes baseline in architecture review, and the owner's go for the deploy. |
| M8 | **Later**: approvals with hooks, pairing, Telegram, slash commands, the neutral `ShellTools` and `HomeChannel` PRs, a Hermes→needletail migration doc. | The owner approves moving further agents, one at a time. |

## 10. First PR scope

The first PR is a draft titled **"needletail: engine adapter skeleton (M0)"**, on branch `needletail`. It has no binary, no wasm and no new Go modules.

**New files**
- `docs/needletail.md`: this document.
- `internal/needletail/wire/wire.go`: Hook v1, Activity, Session, Message, Job, Config, version constants.
- `pkg/engine/needletail/`:
  - `needletail.go`
  - `compile.go`
  - `env.go`
  - `schedules.go`, with the jobs codec shared with `hermes/schedules.go` through a small neutral helper, or ported if sharing would touch Hermes
  - `runtime.go`
  - `conversations.go`
  - `bench.go`
- `pkg/engine/needletail/testdata/`:
  - `home/**` (atlas golden)
  - `classify.json`
  - `hook_v1.json`
  - `jobs.json`
  - `bench.jsonl`

**Modified files**
- `pkg/engines/engines.go`: two lines. The neutral `VersionValidator` lands as its own earlier PR.

**Tests in `pkg/engine/needletail`**

| Test | Covers |
|---|---|
| `compile_test` | Atlas golden. Every emitted file is classified. An unknown reasoning level is refused. Grants render into `needletail.yaml`. |
| `layout_test` | Every runtime path classified; sessions and audit are Raw; `tools/*.luau` is Learning. |
| `env_test` | `AGENT_HOME`, `SWARM_*`, state vars and `SLACK_HOME_CHANNEL` are present. |
| `schedules_test` | render → Read → Merge keeps live state and agent jobs, and repo definitions win. |
| `runtime_test` | Signed hook parse table, including bad signature and duplicate delivery. Activity decode. Wake idempotency header. API key rule. |
| `conversations_test` | Against `httptest`: list, transcript, new (409), chat tip, lineage delete, status mapping. |
| `bench_test` | stream-json parse. |
| `image_test` | `latest` and an empty tag are refused. |

A `pkg/engines` test checks that `needletail` resolves for both `Get` and `Runtime`.

**Must pass:** `gofmt -l .`, `go vet ./...` and `go test ./...`.

**Process:**
- Commits go on the existing `needletail` branch in its existing worktree.
- The branch is pushed and kept open as a draft PR.
- Updating the parent repo's submodule pointer is a separate step, done afterwards.

## 11. Risks and open questions for the owner

**Risks**
1. **Version validation** is Hermes-shaped. The neutral validator PR must land before manifests use needletail.
2. **Schedule semantics drift.** Hermes behaviour (monitor diff, `[SILENT]`, failure streaks) was not written down anywhere. Mitigation (M4): `docs/schedules.md`, read from the Hermes image's `cron/` package, plus table tests from real jobs. Before a cutover, check an agent's jobs against its §8 (a pinned `model`, `context_from`, one-time jobs and `monitor_url` are not supported yet).
3. **Luau wasm toolchain.** Exceptions and longjmp under WASI. Spike before M6.
4. **Parity of loop quality.** Compaction and context management regress as worse answers, not errors. Mitigation: bench plus a soak against Hermes.
5. **Contracts freeze at the first nap.** Review M0 and M1 before any real agent naps.
6. **Hermes vocabulary is entrenched**: `terminal`, `mcp_core_*`, `API_SERVER_KEY`. This is accepted for parity, and the neutral follow-ups loosen it later. Note (M2): the Hermes image `v2026.9.24` registers the core's tools as `mcp__core__<name>`, not `mcp_core_<name>`, and in the example instance offers them to no `api_server` turn. Needletail keeps `mcp_core_<name>` as designed; the wake text names the tools without a prefix, so it reads the same. Prompts or skills that spell out a prefixed name need checking before a cutover.
7. **No approvals in the MVP.** Agents that depend on them stay on Hermes.
8. **Skill scripts depend on Hermes helpers** and `HERMES_*` env. Audit them before each cutover.
9. **The runtime uid stays tied to `shared.PosixUID`** until the mounts plan's step 3.

**Decided**
1. **Websocket for Slack:** `github.com/gorilla/websocket` v1.5.3, pinned (a second module, added in M5 and checked against the two-week rule then).
2. **`/shared` writes from the file tools:** direct, each followed by an automatic `fs_note` to the core (parity, the MVP default).
3. **Luau tool header:** inside the `.luau` file (§5.4). Grants stay outside `skills/`.

**Open questions**
1. **Default Luau grants.** Is `core:msg_*` on by default acceptable, or should every core cap be explicit?
2. **First cutover agent:** the test agent or a named low-risk production agent?
3. **Image registry:** ghcr or ECR?
