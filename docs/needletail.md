# Needletail

Needletail is a Stormo engine written in Go. It runs agents in place of the Hermes image and lets agents write their own tools in Luau. This document is the design and the contract: the formats it defines are frozen at the first nap of a needletail agent (see §8).

## 1. Goal and non-goals

**Goal.** One static Go binary (`CGO_ENABLED=0`) that sits behind the existing `pkg/engine` contract (`Engine` + `Runtime`) and gives an agent everything it has today:
- the agent loop (an OpenAI chat-completions client with core and cloud fallback)
- built-in tools, SKILL.md skills and two hot-memory files (with every memory also a searchable record, §4.6)
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
- Sandboxing the bash `terminal` tool beyond what the container and the uid split provide (see §7): it can still reach the network and the agent's workspace.
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
  messaging/                  the one policy for messages the agent sends on its own (§4.9), and the
                              handler types the gateway calls (§5.5)
  privsep/                    the uid split: the home's layout and the drop to the runtime's user (§7)
  fsx/                        atomic writes, and writes by descriptor below a root (§7)
  slack/slacktest/            a fake Slack (Web API and Socket Mode on the loopback) for tests
  luau/                       wazero host (engine.go), the binding registry (bindings.go), caps,
                              the tool header, the nt bindings (host.go, http.go), tools and scripts,
                              event handlers (handlers.go)
  luau/wasm/                  build.sh (pinned wasi-sdk + Luau tag/sha), src/shim.cpp, luau.wasm (go:embed)
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
| `luau check <file>` | Validates the header and compiles only (a file under `tools/` needs a tool's header, one under `handlers/` a handler's, any other a script's). |
| `luau reference` | Prints the generated reference of the functions scripts get (§5.3) and of the events handlers get (§5.5). |

**Image** (`deploy/needletail/Dockerfile`)
- Multi-stage. The builder is `golang:1.26.2-alpine` pinned by its multi-arch index digest; it cross-compiles `CGO_ENABLED=0 go build ./cmd/needletail` on the build platform for amd64 and arm64. Its context is only `go.mod`, `go.sum`, `cmd/needletail` and `internal/needletail` (`Dockerfile.dockerignore`), so the build fetches no module besides `go.yaml.in/yaml/v3`, `github.com/gorilla/websocket`, `github.com/tetratelabs/wazero` and wazero's `golang.org/x/sys`. `luau.wasm` is committed, so the image needs no C toolchain.
- The final stage is `alpine:3.22` pinned by digest, with `bash`, `python3`, `git` and `ca-certificates`. Existing skill scripts are bash and python, so the `terminal` tool needs them.
- No `USER`: the container starts as root and the runtime splits itself (§7): it lays the home out, then runs as 10000:10000 (`shared.PosixUID`, `wire.RuntimeUID`) keeping only `CAP_SETUID`, `CAP_SETGID` and `CAP_KILL`, and starts what the agent executes as 10001:10000 (`wire.ExecUID`). `AGENT_HOME=/opt/data` (made and owned by 10000 in the image), a `HEALTHCHECK` running `needletail healthcheck` (an HTTP GET; it never boots a runtime).
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
| `MergeSchedules` / `Schedules` / `ReadSchedules` / `DropSchedules` | Same `cron/jobs.json` shape as Hermes (the codec ported, not shared). `DropSchedules` (`engine.ScheduleDropper`) lets rehydrate drop the runtime-made jobs a reviewer rejected, as for Hermes. |
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
- An optional `Layout.ExecUID` (M6b, not made): the uid split needs no core change, since the runtime starts as root and lays the home out itself after rehydrate's `ChownTree` hands everything to `RuntimeUID` (§7). A second uid in the Layout would only matter if rehydrate or `stormo check` had to know it.
- The agent container's `linuxParameters.capabilities.drop` in the ECS task definition, mirroring compose's `cap_drop` (§7, "On ECS"): it changes every engine's rendered task, so it is its own PR.

**Engine-neutral changes in M6b.** None: no file outside `pkg/engine/needletail`, `internal/needletail`, `cmd/needletail`, `deploy/needletail` and `docs` changed. `pkg/loop` (rehydrate's `ChownTree`), `pkg/engine.Layout`, `pkg/local/compose.go` and `pkg/deploy/taskdef.go` are unchanged; Hermes is untouched.

**Engine-neutral changes for the jail (§7).** Two optional fields, default off and left out of what is rendered when off, so Hermes renders byte for byte as before (pinned by `TestComposeReadOnlyRootFollowsTheEngine` and `TestReadOnlyRootFollowsTheEngine`):
- `engine.Layout.ReadOnlyRoot`: the engine writes only to its mounts, so its agent container runs on a read-only root filesystem (compose `read_only: true`, ECS `readonlyRootFilesystem: true`; the sidecars never). Needletail sets it.
- `engine.Activity.Jail` (`jail`, omitempty): an engine's one-line report of how the agent's commands are confined, carried to the office and review; the needletail adapter passes the runtime's `jail` on only as one short printable line. Hermes leaves it empty.

## 4. Runtime

The runtime is the process `needletail serve`, started as root by the image and running as uid 10000 once it has laid the home out (the uid split, §7), with `HOME=$AGENT_HOME`. What the agent executes (the terminal, schedule scripts and monitors) runs as uid 10001, jailed with Landlock to the home and its shared layers when the kernel has it (§7).

### 4.1 Configuration

`$AGENT_HOME/needletail.yaml` is rendered by Compile and read once at boot. It holds:
- `model`: `primary {base_url, key_env, model}` and `fallbacks[]`
- `reasoning`
- `limits {turns, command_timeout, script_timeout, luau_timeout}`: `script_timeout` (default 30 s) bounds a schedule's `.luau` script, `luau_timeout` (default 30 s) a Luau tool the model calls or `luau_test` runs, so an agent whose schedules need many minutes does not give every interactive tool call as long
- `core {mcp_url, ingest_url, key_env: SWARM_CORE_KEY}`, only when on core
- `channels`
- `memory {memory_char_limit, user_char_limit, recall}`: the hot files' budgets (UTF-16 units), and `recall {auto, jobs, max, budget}`, config.yaml's `memory.recall` (§4.6: on for user turns, off for scheduled runs, 5 records, 1,200 units when unset; `max` 1 to 10, `budget` 200 to 4000), omitted when unset
- `skills {generated}`: the skill dirs the build generates from the instance (the knowledge and shared-docs skills, from `CompileContext.Knowledge`), which `skill_manage` refuses to change (from M3)
- `luau {grants, http_secrets, timeout}` (`timeout`, seconds, 1 to 600, is rendered as `limits.luau_timeout`) and `timezone` (the agent's wall clock, an IANA name; unset: the container's `TZ`, else UTC), taken from the agent's `engine/needletail/config.yaml`, which only the adapter reads (a Hermes agent's `timezone:` moves there from `engine/hermes/config.yaml` at cutover)
- `slack {broadcast}`: config.yaml's `slack.broadcast` grant (§4.9: only a `send_message` that sets `broadcast: true` may then ping `@channel`, `@here`, `@everyone` or a user group); omitted when unset
- `schedules {turns, repo}`: config.yaml's `schedules.turns` (1 to 4, the scheduled runs' own turn lane, §4.4; unset: 1) and `repo`, the ids of the jobs agent.yaml defines, which Compile lists: their deliveries are trusted as the repo wrote them, and they never count toward the agent's own 20 jobs (§4.7). An image older than these keys refuses a `needletail.yaml` that has them (its keys are checked)

### 4.2 Secrets

- At boot the binary reads the values it needs (`SWARM_CORE_KEY`, `API_SERVER_KEY`, `SLACK_*`, the provider key) into a secrets struct.
- On Linux it marks itself non-dumpable (`prctl(PR_SET_DUMPABLE, 0)`), so `/proc/<pid>/environ` (the env it was started with, which unsetting does not change) and the rest of its `/proc` entry belong to root. With the uid split the agent's commands are another uid besides, with no capability, so neither the env nor `mem` nor a signal reaches the runtime (checked in Docker, §7). Commands it starts are dumpable again after their `execve`.
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
6. the guide (`loop.Agent.Sections`, `internal/needletail/cli/guide.go`), generated, not written by hand, and golden-tested (`cli/testdata/guide.golden`, at most `MaxGuide` = 7000 bytes, held by `render` itself: the agent's paths are clipped to 160 bytes and its grants listed up to 900 bytes, then counted with a pointer to `skill_view` (each tool's effective caps) and `luau_reference`; past the bound the compact function reference is cut after a whole line with a pointer to `luau_reference`, the messaging rules after it kept whole (were the rest still over, it would be cut at a line too); about 6.2 KB with Luau, Slack and recall, 1.3 KB without Luau and Slack): where the home's files are and which call changes each protected one (SOUL.md read-only; memories through the memory tools; `cron/jobs.json` through `cronjob_manage`; skills through `skill_manage`; `sessions/`, `logs/`, `state/`, `.swarm/`, `needletail.yaml` never), what the uid split means for the terminal when it is on; how memory works (the hot files and the archive, `memory_manage` and its dedupe answers, bilingual `memory_search` queries, and the recalled block when recall runs); with Luau, a minimal tool and a minimal handler with their headers, each event's answers, caps and the agent's own grants, the limits (`luau_timeout` for tools and the other handlers, `script_timeout` for `on_schedule` handlers and `.luau` job scripts), and the compact function reference (`luau.CompactReference`, one line per function); with Slack, the messaging rules (targets, broadcast, a scheduled run's reply is its delivery). The full reference is `luau_reference` (§4.5). It is rendered at boot and again once the Slack gateway runs.
7. a channel's guidance (`loop.Agent.Sections`): with the Slack gateway running, when the agent may stay out of a group conversation (§4.9)
8. the current time, on the agent's wall clock (§4.7)

**Turn behaviour**
- The loop runs until the model makes no tool calls, `limits.turns` is reached, or the turn is cancelled.
- **Recalled memory** (`loop.Agent.Recall`, `memory.Recaller`): as a turn starts, the archive records most related to its input (§4.6) are put in front of the turn's user message, fenced in `<memory-context>` as background data, in every request of the turn. Never in the system prompt (a per-turn block there would change the prefix every turn and defeat the prompt cache) and never in the session file (transcripts stay what people wrote; the same client text is not copied again into Raw history). A scheduled run tells the recall so (`loop.Options.Job`); recall is off for those unless `memory.recall.jobs`.
- Tool calls run sequentially in the MVP.

**Compaction**
- Triggered when the token estimate passes 80% of `context_length` (from `/v1/models`).
- Older turns are summarised into a **new** session with `parent=<old id>`, and the old session ends with `end_reason=compression`.
- This matches today's lineage, `Tip` and `Compacted` semantics.

**Concurrency**
- One running turn per conversation, in two lanes: two chat turns (the API's and Slack's, `api.Server.Reserve`) and the scheduled runs' own (`ReserveJob`, `schedules.turns`, one by default), so a burst of jobs never starves chat nor chat the jobs.
- Over a lane's cap the API returns 429 (`ErrBusy`). A run on a busy session returns 409.

### 4.5 Built-in tools

Names stay Hermes-compatible, so bench files, `schedules.tools`, `ShellTools` and the wake text keep working.

| Tool | Behaviour |
|---|---|
| `terminal` | `bash --noprofile --norc -c` with cwd `$AGENT_HOME`, `command_timeout`, capped output, and an explicit environment: the process env without the runtime's own secrets, plus the passthrough allowlist, `HOME` the agent's home and `PATH` the env's absolute entries (a relative one, `.` or empty, would run a planted file from the home by a bare name), else the login default `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` (Alpine's `/etc/profile`). It is not a login shell: no startup file (`/etc/profile`, `~/.bash_profile`, `~/.profile`, `~/.bashrc`) runs before a command, so a file planted in the home never runs on its own; an agent that relied on its own `~/.profile` sources it in the command. At the timeout (or when the turn is cancelled) the command's whole process group is killed. A process the command leaves running in the background after a normal exit (`cmd &`, say a server the agent starts on purpose) is **not** killed: once the command has exited, the tool waits 2 s for the output to close, then returns the output so far and the exit code with a note that background processes are still running; what they write later is not captured. When it runs from a skill it exports `SKILL_DIR` (the skill's absolute directory) and counts a use of it: the skill the call names (`skill`, a name or dir), else the one skill whose directory the command names (`skills/<dir>/…`, absolute or relative, with `skills/` at a path boundary, so `/tmp/otherskills/<dir>/` is not it), else, for a command that expands `SKILL_DIR` (`$SKILL_DIR` or `${SKILL_DIR}`; a command that only names it, such as `grep -r SKILL_DIR skills`, does not), the skill the session last viewed (kept in memory, for at most 256 sessions, and carried to the child a compaction continues the session in; a restart forgets it). A command that expands `SKILL_DIR` with none of these is refused rather than run with it empty. With the uid split (§7) the command runs as 10001:10000, with no supplementary group and no capability, and umask 002; the runtime keeps `CAP_KILL` to kill its process group. It starts jailed (§7, "The jail"): through `needletail jail`, which confines itself with Landlock to the home and the shared layers and then execs bash, so the command and all it starts can read and write only there, and read and run the system's programs; `TMPDIR`, `TMP` and `TEMP` are `$AGENT_HOME/.tmp` (a schedule's script gets the same). The environment (the terminal's and a schedule script's, `tools.CommandEnv`) also sets git's `safe.directory=*` through `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>`, after any entries the env already sets that way: the commands work in directories the runtime made (a new skill's, those `write_file` made, the home itself; on ECS all of `/shared`, which EFS stores as 10000), and git refuses a repository another user owns. Trusting them gives nothing away: what a repository's config could run, it runs as the commands' own user, which can already run anything. |
| `read_file`, `write_file`, `patch`, `search_files` | Paths are confined by `sandbox.Resolve` to `$AGENT_HOME` or `/shared/{group,<unit>}`, with symlinks evaluated and re-checked; a path must resolve inside the same root it names, so a symlink from one root into another (say `/shared/group/x` → the home's `skills/`) is refused. Only regular files are opened (a FIFO would block the turn), by descriptor from their root with `O_NOFOLLOW` on every component (`read_file`, `patch`'s read and `search_files`' reads, as writes are: a directory swapped for a symlink after the path was resolved fails the read instead of reaching `sessions/` or `logs/`), and a write truncates only after checking that the path still resolves to the file it opened. `search_files` walks its tree by descriptor too (`fsx.WalkBelow`: each directory opened relative to its parent with `O_NOFOLLOW`, a symlink never entered), so its exclusions hold while the agent's commands race it. `read_file` holds at most one capped line in memory; `patch` refuses files over 8 MiB. Direct writes to `/shared` are allowed for parity and followed by an automatic `fs_note` (decided, §11; §4.11); prompts prefer `fs_write`. The runtime's own files and what the managing tools keep are never written by these tools (compared as Unicode case folds, `strings.EqualFold` on the first segment: `Cron/` and `ſtate/` are `cron/` and `state/`, as a case-insensitive home may take them): `sessions/**`, `state/**`, `needletail.yaml`, `cron/**` (`cronjob_manage`'s, so its bounds and the messaging policy for an agent job's targets cannot be skipped), `logs/**` (the Luau audit), `.swarm/**` (`booted.json`, by which `skill_manage` keeps repo skills), `SOUL.md` (changed only through review), `memories/**` (only through the `memory` tool, with its bounds and scrub) and `.needletail-tmp/**` (the runtime's temp dir); `search_files` skips `sessions/`, `state/` and `logs/` and refuses to start in one. `scripts/` stays writable: the agent writes a job's script before naming it. With the uid split the terminal reaches none of these either (§7). A write reaches its file by descriptor from its root (`fsx.OpenFileBelow`: the home or the shared layer, each directory opened with `O_NOFOLLOW`), so a directory the agent's commands swap for a symlink after the checks fails the write instead of sending it into `cron/` or `state/`; a file it makes is 0664 and a directory 0775 (group-writable, so the agent's commands can change them). |
| `memory` | Hermes' schema and answers: add, replace and remove on the `memory` or `user` target, by text (`old_text`, a unique part of the entry). The budget is counted in UTF-16 units; when over, it returns an error showing the current usage. It works on the records (§4.6): `add` makes a hot record (kind `fact`, or `user`), or pins the archived record with the same normalized text instead of copying it; an entry already there is a no-op; `replace` changes the record's text; `remove` deletes the record. A secret value the runtime knows is refused (as in `skill_manage`), and so is text that reads as an instruction to drop the model's rules or reveal secrets (Hermes' heuristic for what enters the system prompt) or holds invisible or bidi characters. |
| `memory_search` | The archive by keyword (§4.6): `query` (plain words, ORed; `"a phrase"`; `word*` a prefix; empty lists newest first), filters `kind`, `tags` (all), `scope` (`all`, `hot`, `archive`), `since`, `include_expired`, `match` (`any`, `all`), `limit` (8, at most 20); `ids` fetches up to 20 records whole (a use of each). Answers `{total, results: [{id, kind, tags, hot, imp, created, updated, expires, expired, pii, src, score, text, cut}]}`, texts cut to 600 UTF-16 units in searches. Offered to scheduled runs (the `memory` toolset and the default set). |
| `memory_manage` | The archive by id: `save` (`text`, `kind` fact/preference/procedure/episode/user, `tags`, `importance` 1-5, `expires` YYYY-MM-DD, `hot`, `force`) answers `created` (id), `exists` (the same normalized text: its id, touched) or `similar` (records whose words contain the new text's, or are contained in them, at 75% or more: nothing saved, the candidates listed, so the model merges with `update` or saves again with `force`); `update` (id and the fields to change; `expires: ""` clears it; a kind change between `user` and the others moves a hot record between USER.md and MEMORY.md); `delete`; `pin` (into the hot file, at its end; refused over budget, with the usage, and by the injection heuristic); `unpin`. Texts are 1 to 1,000 UTF-16 units, no control characters but newlines and tabs, no runtime-known secret; at most 8 tags (`[a-z0-9][a-z0-9_-]{0,31}` after folding); at most 20,000 records. Never offered to a scheduled run unless its list names it. |
| `skills_list`, `skill_view`, `skill_manage` | Hermes' names and arguments. A skill is named by its dir under `skills/` (`crm/crm-api`), its front-matter name or its dir's last segment; an ambiguous name is refused with the candidates. `skills_list` lists name, description, dir and `read_only`. `skill_view {name, file_path?}` loads progressively: SKILL.md (or one file of the skill, at most 100 KB, binary refused) with `${SKILL_DIR}` resolved to the skill's absolute directory, plus the skill's other files (hidden files and nested skills left out). `skill_manage {action, name, …}`: `create` (name and optional one-segment `category`, both `[a-z0-9][a-z0-9_-]*`; `content` is the whole SKILL.md and must start with front matter naming the skill and describing it), `edit`, `patch` (`old_string` must match once unless `replace_all`; `file_path` omitted is SKILL.md), `write_file`, `remove_file` (empty directories it leaves are removed) and `delete` (one rename takes the skill out of `skills/` into `.needletail-tmp/`, then its files are removed, so a nap never sees half a skill). `delete` refuses a skill the instance booted with from the agent's repository (a dir in `.swarm/booted.json`'s `skills`, which rehydrate writes): rehydrate would bring it back at the next restart and the dream proposes no deletion, so removing it is a repo change a person makes; the agent may still change it. It refuses the build's generated skills (`skills.generated`, by dir or by name anywhere, since the dream skips them by name), hidden dirs, symlinks anywhere below the home, `..` and absolute paths, control characters in a path, a file named `SKILL.md` in any case other than the skill's own (`write_file` cannot make a nested skill that skips `create`'s checks), a file of a nested skill, a rename (the front-matter name must stay), a description over 1024 characters (unless it is the one the SKILL.md already had), files over 1 MiB, and a secret value the runtime knows anywhere in the call (`name`, `category`, `file_path` and the texts) or in the file a write would leave (the library's `Check` sees the whole patched file, so a secret split across two patches is refused too). Since a view shows `${SKILL_DIR}` resolved, what `create`, `edit`, `patch` and `write_file` write has the skill's absolute directory (followed by `/`, a quote, a space or the end, never a longer name that starts like it) put back as `${SKILL_DIR}`, and `old_string` is matched in that form first, then as given (a file that holds the literal path). A script therefore finds its directory in the `SKILL_DIR` env var (bash `$SKILL_DIR`; Python `os.environ["SKILL_DIR"]`), which the terminal exports. Repo skills may be changed (the dream proposes them as changed). Writes are atomic and reach their directory by descriptor (`fsx.WriteAtomicBelow`; `remove_file` and `delete`'s rename too, `fsx.RemoveBelow`, `fsx.RenameOut`), so a directory swapped for a symlink fails them; files are 0664 and directories 0775, group-writable for the agent's commands (§7). A `.luau` file may be a tool, `tools/<name>.luau`, or an event handler, `handlers/<name>.luau` (§5.5), and every write, edit or patch of one is checked on the whole file it would leave: the header (§5.4, §5.5, its name the file's) and a compile. A view of a SKILL.md lists the skill's tools and handlers: each registered name with its declared and effective caps (and a handler's events and jobs), or the error that keeps it out. `read_file` of a SKILL.md counts as a view. |
| `luau_test` | Dry-runs a Luau tool (`{tool: <registered name or file>, args}`) with its effective caps but writes, http, core calls and sends refused (`dry_run`), so the agent can iterate. Nothing reaches the core: a `/shared` `list` or `stat`, which the core answers in a real run, reads the mount instead (refused when it is not mounted). It answers the result or error, logs, output, refused calls and caps, and for an invalid tool its error. A handler (`{tool: skills/<skill>/handlers/<name>.luau or <skill>__<name>, event?, args?}`) is called with a sample event of `event` (its first by default) with `args` laid over it, and the answer is read as the runtime reads it: the decision, or why the runtime would skip the handler. |
| `luau_reference` | The full reference (`luau.Reference()`: every function with its arguments, result, grant and an example, and every event with its fields and answers); the system prompt carries the compact form (§4.4). Registered with the Luau engine. |
| `send_message` | Hermes' tool for Slack (§4.9), under the runtime's one messaging policy (the agent's `before_send` handlers see the message first, §5.5): posts to the home channel, a channel or DM the agent has a conversation in, a thread it is engaged in, or the DM of a person `allowed_users` names; `broadcast: true` keeps `@channel`/`@here`/`@everyone`/user groups only with the `slack.broadcast` grant; `list` shows the targets. Registered only while the Slack gateway runs; never offered to a scheduled run. |
| `no_reply` | Stays out of a Slack conversation with other people (the same as a `[SILENT]` reply, §4.9); refused in a DM, when the agent was mentioned, and outside Slack. Registered only while the Slack gateway runs; never offered to a scheduled run. |
| `cronjob_manage` | Hermes' scheduling tool (its name, toolset `cronjob`, and its arguments where they mean something here): `list`, `create`, `update`, `pause`, `resume`, `remove` and `run` (due now) on the agent's jobs and the repo's. One-time schedules are refused (agent.yaml cannot hold them). `deliver` and `failure_deliver` are held to the messaging policy when the agent sets them (§4.9). Never offered to a scheduled run. See docs/schedules.md §7. |
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
- The generated knowledge and shared skills arrive through `CompileContext.Knowledge`; Compile lists their dirs in `needletail.yaml` (`skills.generated`), and the runtime shows them like any skill but never changes them. It also names the manifest's state dir there (`state_root`, the instance's `names.state_dir`), which the uid split keeps reachable (§6).
- What the agent writes under `skills/[!.]*/**` is Learning: the nap keeps it, rehydrate brings an agent-created skill back always and a changed repo skill back while the repo has not moved under it, and the dream proposes both (new or changed) with no change to `pkg/loop`. `skills/.usage.json` comes back through rehydrate's `skills/.*` rule and is copied by the dream.
- What keeps the agent's skills from harming its own turns or the review is in §4.4 (the bounded skills index in the system prompt) and §4.5 (`skill_manage`'s refusals: descriptions over 1024 characters, a nested `SKILL.md`, control characters in paths, secrets anywhere in the call or the file it leaves; `delete` of a repo skill, which rehydrate would restore). `serve` empties `.needletail-tmp/` at boot (§6).
- Memory has two tiers (`internal/needletail/memory`). **Hot**: `memories/MEMORY.md` and `memories/USER.md`, entries separated by `\n§\n`, with no trailing newline, in every system prompt. The seed is merged by the existing rehydrate code. Entries are trimmed of the same characters the dream and rehydrate trim (every Unicode space and the BOM; `strings.TrimSpace` would differ on U+0085 and U+FEFF), and both budgets count UTF-16 units of the whole file, so the memory tools, the merge at boot and the ledger see the same entries within the same budget (`TestMemoryParity`). **Archive**: `memories/RECORDS.md` (format v1, §8) holds every memory, hot or not, as a record: `id` (`m` + 6 time-ordered and 5 random base-36 characters), `kind` (`fact`, `preference`, `procedure`, `episode`, or `user`, which renders to USER.md and becomes the ledger's never-promoted `KindUser`), `tags`, `imp` (1-5), `hot` (its position in its hot file; 0 is the archive), `src` (`chat:<session>`, `luau:<tool>`, `migration`, `boot`), `created`, `updated`, optional `expires` and `pii` (the kinds `pkg/learning`'s generic scrub rules find: a copy, parity-tested; informational). RECORDS.md is the runtime's source of truth and stays plain and readable (`cat`, `grep`; a Markdown viewer hides the headers).
  - **Rendering.** The hot files are rendered from the hot records, in `hot` order, after any change to a hot record, byte-identical when the set is unchanged (`render(parse(f)) == f`, tested). Every change rewrites RECORDS.md first, then the hot files it touched, each atomically through `.needletail-tmp/` (0640); one mutex per store serialises concurrent conversations.
  - **Reconcile.** Whenever one of the three files changed on disk since the store last read or wrote it (at boot, after rehydrate; when another process wrote them), the records are reconciled with the hot files, and the hot files win for the hot set: each entry, in order, takes the record of its target with the same `Normalize`d text (a copy of `pkg/learning.Normalize`, parity-tested; a hot record first, else the latest updated archived one) and its position, and the hot file's spelling; an entry no record has becomes one (`src: boot`; every entry, with `src: migration`, when RECORDS.md is missing: the first boot of this runtime, or an agent coming from Hermes; the hot files are not rewritten); a hot record its file no longer has goes to the archive (`hot: 0`). **No path drops a record.** Without the merge report the design proposes (not built: §11) a record a reviewer rejected cannot be told from a nap skew, so it is kept in the archive too. An entry of RECORDS.md that is not a record is kept verbatim after the records and logged; a file of a newer format makes memory read-only.
  - **Search.** An in-memory BM25 index (k1 1.2, b 0.75), built at boot and changed in place on every write, never napped: text lower-cased with diacritics and Latin ligatures folded (cotización matches cotizacion), split at anything not a letter or digit, words of one character and about 230 Spanish and English stop words dropped, no stemming; tags count twice. The best 50 BM25 candidates (after the exact filters) are re-ranked by `0.65·lex + 0.15·recency + 0.12·importance + 0.08·use`: `lex` BM25 over the best candidate's, recency `0.5^(days since max(updated, last use)/30)`, importance `(imp−1)/4`, use `min(1, log2(1+uses)/log2(17))`. A use is a fetch by id, an update or a pin; being recalled is counted apart (`recalls`), so recalled records do not become more recallable. The counters live in `state/needletail/memory-usage.json` (ids and numbers, no text; written at most once a minute, at shutdown and after a oneshot), never in the readable file.
  - **Recall** (§4.4). For a turn's input (its last 2,000 characters): nothing for an acknowledgement or greeting (`ok`, `gracias`, `sí`, `thanks`, …), for fewer than two content words or an empty archive. Otherwise archive records (not hot, not expired, not already recalled in the conversation, kept per root session for 256 conversations) with `lex ≥ 0.35` that hold 30% of the query's distinct words or a rare one (in at most 1% of the records), packed best first while a record's words overlap less than 0.6 (Jaccard) with one already picked, up to `memory.recall.max` records (5) and `budget` UTF-16 units of lines (1,200), each text cut to 400. A line is `- <id> · <kind> · <updated date>: <text>`, `(until <date>)` with an expiry, `(saved by a tool)` for a Luau record.
  - **Measured** (Go benchmarks, `internal/needletail/memory/bench_test.go`, 10k synthetic records of 25-75 Zipf words, Apple M4 Max): index build 54 ms (12 MB), boot (parse, reconcile, index) 95 ms, search 0.27 ms with a very common word and 0.045 ms without, recall 0.08 ms, one save (RECORDS.md, 5 MB, rewritten and fsynced) 17 ms. ECS vCPUs are expected 1.5 to 3 times slower.
- Conversation lineage (a compaction ends a session and continues it in a child, §6) is Raw: it comes back verbatim at rehydrate, and the dream never reads it.

### 4.7 Schedules (`sched`)

`docs/schedules.md` is normative: it writes down Hermes' semantics (read from the image's `cron/` package) and lists where needletail differs. As built in M4:

- **File.** `cron/jobs.json` in the `{"jobs":[…]}` shape the adapter renders and Hermes writes, read and written as ordered raw objects, so keys the runtime does not model survive and an unchanged file is written back byte for byte. Needletail adds one state key, `running`, and keeps Hermes' `monitor_state` and `failure_streak`. The scheduler is the file's single writer: its tick, its runs and the `cronjob_manage` tool change it under one lock, re-reading it first, and write it through `fsx.WriteAtomicIn` and `.needletail-tmp/`.
- **Timing.** `interval` (minutes after the last run) and 5-field `cron`, parsed in-house (`sched.ParseCron`: lists, ranges, steps, names, Sunday as 7, Vixie's day-of-month/day-of-week OR) on the agent's wall clock (`needletail.yaml`'s `timezone`, else `TZ`, else UTC; the binary embeds the zone database), which the system prompt's current time uses too. The tick runs every 30 s; a due job runs once however many of its times passed; `next_run_at` moves before the run starts and `running` is set, so a crash never runs it twice, and boot records a run a restart cut short as failed.
- **Turns.** An agent job's turn takes a slot in the scheduled runs' own lane (`api.Server.ReserveJob`: `schedules.turns`, one by default, beside the chat's two, §4.4). A job without scripts reserves it first, and a busy lane leaves it due for the next tick; a job's monitor and scripts run before any slot is taken, at most two at a time (`sched.MaxGates`; another waits), and a busy lane after a gate that wants a turn leaves the job due again with the monitor's output not stored, keeping its gate's result in memory (the blocks for the prompt and the monitor's pending store) until it gets a slot or its schedule's next time passes (then its gate runs again, the job keeping its place), so its scripts and handlers do not run again every tick; an update, pause or removal while a gate runs drops the result it leaves (checked under the file's lock against the job's definition). Due jobs start the longest due first (the tick sorts them by `next_run_at`), and a job that found the lane busy holds its place in line (`waiting`): no job due after it reserves the lane while it waits (a later gate that ends meanwhile waits behind it, logged once, and kicks the ticker so the older one tries at once). Every job is compared at its own place in line (the earlier of the one it recorded and its due time), and `run` on a job already due leaves its `next_run_at` as it is, so two waiting jobs never each see the other ahead. A job never runs twice at once: a start claims it before anything of it runs, a tick while it runs leaves it alone, and `run` requests made while it runs coalesce into one more run (a busy outcome takes them: the job is due anyway); the run's record takes the requests and lets the job go in one step under the file's lock, so a `run` asked while a run ends is neither lost nor run twice, and one asked between the claim and the start is the run that starts. The run is a turn in its own session `cron-<id>-<UTC time>` with source `cron` (the hooks' platform), created only when the turn runs and ended (`cron_complete`) after it. Script-only jobs take no slot.
- **Run.** Monitor first (exact-output hash in `monitor_state`, snapshot in `state/needletail/monitor/<id>.out`; unchanged is `no_change`; the baseline or the unified diff and current output go into the prompt; state stored once the turn slot is held, before the turn), then the pre-run script (its output, or its error, in the prompt; empty output or `{"wakeAgent": false}` is silent), then the agent's `on_schedule` handlers whose `jobs:` name the job (§5.5: a reply is the run's output with no turn, silent a silent run, a pass's notes go into the prompt as a "Handler Notes" block), then the prompt: each skill loaded with `skills.Library.View` for the run's session (a counted view, the session's current skill for `SKILL_DIR`), a short cron hint, the script block, the job's prompt and the monitor block. `enabled_toolsets` expand Hermes' toolset names (`skills` → skills_list, skill_view, skill_manage; `file`; `terminal`; `memory`; a tool's own name) before `loop.Options.Tools` filters, the core's `mcp_core_*` tools stay unless `no_mcp`, and `cronjob_manage` and `send_message` are never offered to a run. `model`/`provider` are not applied yet (logged).
- **Outcome.** `ok`, `silent` (`[SILENT]` as the reply or its first or last line), `no_change`, `error` (a failed turn, `[CRON_FAILURE]`, an empty reply, a failing monitor or script-only script, an unrunnable job, which is also paused) and `delivery_failed`. The failure streak counts errors and any success resets it; from the third a notice says the job is worth a review. `repeat.times` completes the job. Every run that did something is recorded in `state/needletail/jobs/<id>/<time>.md` (the newest 10).
- **Delivery.** `deliver` and `failure_deliver` are parsed into targets (`local`, `origin`, `slack`/`home`/`all`, `slack:<channel>[:<thread>]`) and handed to a `sched.Deliverer`: the Slack gateway (§4.9) when it runs, else `sched.Pending`, which accepts and logs a Slack target (the output stays in the run's record, and `last_delivery_error` stays empty so review does not flag every job); another platform is a delivery error. A job agent.yaml defines (`schedules.repo` lists it, or it has no `created_at`) is trusted: its targets are the repo's. Any other job, one the agent made, delivers only where the messaging policy allows (§4.9), checked when `cronjob_manage` sets a target (`origin` as the job's own origin) and again at every delivery (a refused target is `delivery_failed`). Because a repo job's deliveries are trusted, `cronjob_manage` refuses to change what it runs (`prompt`, `skills`, `script`, `monitor`, `no_agent`, `enabled_toolsets`) or its `name` (by which a handler's `jobs:` names it), pointing at a job of the agent's own; its timing and targets (policy-checked) can change, and it can be paused, resumed, removed and run. (The agent can still shape a repo job's output through what the run reads: its skills, memory, handlers and the files of `scripts/`; trust covers where it goes, not what it says.) A failing script's or monitor's notice carries at most 16 KB of its output.
- **Scripts** run from the home's `scripts/` (symlinks resolved and confined), `.sh`/`.bash` with bash, `.luau` in the Luau sandbox (§5; its header required, its bound `limits.script_timeout`; docs/schedules.md §5) and anything else with python3, cwd the script's directory as in Hermes, the terminal's environment, a 1 h bound and a process-group kill; with the uid split as the agent's user, 10001:10000 with no capability, as the terminal (§7).
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

The gateway (`internal/needletail/slack`) follows Hermes' Slack platform (the image's `plugins/platforms/slack`). `serve` starts it when `needletail.yaml` lists the `slack` channel and both `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` are set; its settings are the channel's, in the env Hermes reads (`SLACK_ALLOWED_USERS`, `SLACK_ALLOW_ALL_USERS`, `SLACK_ALLOW_BOTS`, `SLACK_HOME_CHANNEL`), plus `needletail.yaml`'s `slack.broadcast` grant.

**Socket Mode.** `auth.test` gives the bot's own user and bot ids (retried until it answers; only `invalid_auth`, `not_authed` or `account_inactive` make the state `fatal`, a rate limit or Slack's own trouble does not); `apps.connections.open` with the app token gives the websocket URL (`github.com/gorilla/websocket` v1.5.3, decided in §11, released 2024-06-14). An envelope is acked once its message is queued (Slack sends an unacked one again after a few seconds); `disconnect` (`refresh_requested`, `warning`, `link_disabled`) and a dead link (pings every 30 s, a read deadline of 70 s) are answered with a new connection, with backoff from 1 s doubling to 1 min, reset by a `hello`. Refused tokens are state `fatal`. The socket's reader only routes an event (no Web API call), in Slack's order; slash commands and interactivity are acked and ignored. `NEEDLETAIL_SLACK_API_URL` overrides the Web API URL for tests only: it must be a loopback address (and only then is a `ws://` link accepted), so the tokens never reach another host.

**Who is answered.**
- `message` and `app_mention` events of Hermes' subtypes (plain, `file_share`, `thread_broadcast`, `me_message`, `bot_message`); edits, deletions, joins and the like are not answered. Each `channel:ts` is handled once (Slack's retries, and the `app_mention` twin of a channel message); it counts as handled only once queued, so the retry of one a full queue refused is not taken for a duplicate, or once `allow_bots` refused it for good (`none`: a bot's `app_mention` twin does not get the turn its message was refused). Under `mentions` a refused twin is not marked: whether a message mentions the agent is read from the message itself, its text (`<@id>`) or its blocks (a rich-text user element), never from which twin it is, so a message and its twin get the same answer whichever Slack sends first.
- The agent's own posts never. **`allowed_users` is the gate for people and bots alike**: a message gets a turn only when `SLACK_ALLOWED_USERS` lists its author's user id (or, for a `bot_message` with no user, its bot id `B…`), or `SLACK_ALLOW_ALL_USERS` is set; anyone else is ignored (logged once). A mention alone is never enough. `SLACK_ALLOW_BOTS` then filters the bots the list lets through: `none`, `mentions` (the default: only when they mention the agent) or `all`. To let agents talk to each other, list each other's bot user ids in `allowed_users` (and keep `allow_bots: mentions` or `all`).
- DMs (`im`) always; channels and group DMs on a mention, and replies in a thread the agent is in without one: a thread it posted in, was mentioned in, or has the session of (so the rule survives a restart).

**Turns.** A channel thread is a session `slack-<channel>-<thread_ts>` (a top-level message starts its own thread: the reply goes under it, as Hermes replies). **A DM is one continuing conversation per person**, `slack-<channel>-dm`: its replies are posted in the DM itself, or in a DM thread when the person wrote in one (a DM thread is the same conversation, so the agent never loses the thread of a DM). Source `slack`; `slack.Origin` reads both forms for `cli.origin`, so a job made in a thread delivers back to that thread and one made in a DM to the DM. Each conversation has its own worker: one turn at a time per conversation through the API's chat lane (§4.4); a message that arrives while its conversation's turn runs goes into the next turn (merged, as Hermes queues follow-ups); a busy agent is waited for (every 2 s, up to 10 min), then a short note says it could not get to the message. The worker, not the socket's reader, reads names (`users.info`, cached; at most 20 lookups per message, the rest shown by id) and a thread's earlier messages, so a slow lookup holds up only its own conversation. A conversation queues at most 32 messages and the gateway 512 in all, counting both the messages waiting and those a turn that has not returned carries (so mentions spread over many threads cannot start a worker each past the cap): an event that finds its queue full is **not acked**, so Slack sends it again. A turn (or a handler) that panics gives its slot back and fails only its own batch (❌ on its DMs and mentions, logged): the worker goes on to the messages queued behind it. The input starts with `[Slack message from <name> (<user id>)]`, the agent's own mention removed and others shown as `@Name`; a reply in a thread the agent has no session in yet starts with the thread's earlier messages (`conversations.replies`, at most 30, Hermes' format: `[thread parent]`, `[assistant]`, and `[unverified]` for every author `allowed_users` does not list, people and bots alike, who may not instruct the agent). The runtime's markers (`[Slack message from`, `[Thread context`, `[End of thread context]`, `[thread parent]`, `[assistant]`, `[unverified]`, `[Handler notes`) inside quoted text and names are neutralised (the bracket becomes `(`), so a quoted line cannot pass for one, however it is spelled: any case (Unicode folds: `ſ` is `s`), any spacing or none between the words, format characters (zero-width spaces and joiners, soft hyphens, bidi controls) ignored, fullwidth letters and lookalike brackets (`［`, `⟦`, `【`, …) read as ASCII; the rest of the text is kept as written.

**Staying out of a group conversation.** In a channel or a thread with other people the agent may choose not to answer: a reply that is `[SILENT]` (the scheduler's rule: the whole reply, or its own first or last line; `SILENT`, `NO_REPLY`, `NO REPLY` too) or a turn that called `no_reply` posts nothing and leaves no reaction (the log says so, naming the tools the turn used). A DM or a message that mentions the agent is always answered: `no_reply` is refused there, a `[SILENT]` first or last line is dropped and the rest posted, and a reply that is nothing but the token posts nothing, is logged, and has its 👀 removed with no ✅ or ❌ (decided: the model's choice is honoured rather than a placeholder posted, and the log shows it). The system prompt's Slack section (§4.4) says when to skip: people talking to each other, a thanks that needs no answer, a question someone else is answering.

**Replies.** In the thread (or the DM), redacted, converted to Slack's mrkdwn as Hermes converts Markdown (tables as aligned code blocks, code kept, links `<url|text>`, headings and bold as `*bold*`, italics, strike), split into numbered parts of at most 39 000 characters with code blocks closed (right after their last line) and reopened, never inside inline code. `&`, `<` and `>` are escaped in all text, code included, but for Slack's own `<@user>`, `<#channel>` and `<url>` tokens the model wrote; a Markdown link's URL has `<`, `>` and `|` percent-encoded and its label escaped, and only `http(s)` and `mailto` links become links. Every special mention (`<!channel>`, `<!here>`, `<!everyone>`, a user group's `<!subteam^…>`, `<!date…>`) is escaped, so model text never pings a crowd; only `send_message` with `broadcast: true` and the grant keeps broadcasts and user groups (and turns a plain `@here`, `@channel`, `@everyone` into one); `<!date…>` is escaped always. Each message a turn answers that is a DM or a mention gets 👀 while the turn runs, then ✅ or ❌ (every one of a merged batch, not only the last; ❌ on each when the shutdown leaves the batch unanswered); the assistant status line says "is thinking..." (turned off for the process when Slack refuses it). A failed turn answers with a short error (redacted, at most 300 characters).

**The messaging policy** (`internal/needletail/messaging`) is the one rule for every message the agent sends on its own: `send_message`, Luau's `send_message` (§5.3) and the deliveries of the jobs it made (§4.7). Allowed by default: the home channel (and its threads), a channel or DM the agent has a conversation in, a thread it is engaged in (it has the thread's session, posted there or was mentioned there), and the DM of a person `allowed_users` names by id (`*` and `allow_all_users` make no one a DM target). A DM channel id (`D…`, a job made in a DM delivers to one) is its person's DM: a target only when that person is listed by id, whatever conversation the agent had there; the person is the author the gateway saw write there, else Slack's `conversations.info` (the `im:read` scope; a DM whose person cannot be told is refused). A handler's reply in a DM it was written in is posted as the turn's reply would be. Anything else is refused (`messaging.ErrRefused`). A broadcast needs both the call's `broadcast: true` and the agent's `slack.broadcast` grant (engine/needletail/config.yaml); Luau and deliveries never broadcast.

**Delivery.** The gateway is the scheduler's `sched.Deliverer`: a target with no channel is the home channel, `slack:<channel>[:<thread>]` a channel or thread, a user id their DM (`conversations.open`); a job the agent made reaches only what the messaging policy allows, a repo job's targets are the repo's (§4.7); output is posted as it is (Hermes' `cron.wrap_response: false`), never broadcasting, `[SILENT]` never; a Slack error or a refused target is the run's `delivery_failed`. A reply in a delivered message's thread wakes the agent there. Without a gateway the deliverer stays `sched.Pending`.

**`send_message`** (Hermes' name and arguments, Slack only, plus `broadcast`): `send` to `slack` (the home channel), `slack:<channel>[:<thread_ts>]`, or `slack:<user id>`, under the messaging policy; `list` shows the targets. Never offered to a scheduled run.

**The agent's handlers (M6b, §5.5).** Each message the gateway answers, once it passed `allowed_users` and dedupe (an unauthorised one never reaches a handler), goes to the agent's `on_message` handlers, or `on_mention` ones when it mentions the agent (one event per message), in its conversation's worker before a turn slot is taken; messages merged into one turn get one event each. The event carries the message (its text with the agent's own mention removed and others as `@Name`, the author's id and name, `bot`, `verified: true`) and, for a reply in a thread the agent has no session in, the thread's earlier messages (fetched once for the handlers and the turn), each flagged `unverified` when its author is not in `allowed_users`, as the turn's `[unverified]` lines are. A `reply` is posted in the message's conversation with no turn (before_send, then the messaging policy, which counts the conversation the agent was written to as one it is in; no reaction); `silent` leaves the message unanswered as a `[SILENT]` reply would (in a DM or a mention, logged); a `pass` keeps the message for the turn with the handlers' notes under it, in a `[Handler notes — …]` block (that marker is neutralised in quoted text like the others). `before_send` sees every message the gateway posts: the turn's reply (`reply`), a handler's (`handler_reply`), `send_message`'s (`send`), a job's delivery (`delivery`) and the runtime's own busy and error notes (`notice`), redacted, before it is formatted and split; a rewrite changes only the text (never the target nor whether it broadcasts: a rewrite of a message without the broadcast grant has its `<!…>` escaped like any model text), a block keeps it back (a turn's reply: nothing posted, the 👀 replaced by ❌; `send_message`: an error, `blocked`; a delivery: `delivery_failed`; a notice: not posted, logged), and so does a `before_send` handler that fails (§5.5: it fails closed), except for the runtime's own notices (its busy and error notes, a job's failure or auto-pause notice), which a failed handler does not keep back. The messaging policy then decides the target as before, after the handlers.

**Shutdown.** New events are left unacked (Slack sends them again to the next runtime); each message still queued gets a ❌, so its author knows it will not be answered; running turns drain with the API's, their replies are posted, then the socket closes.

**Not built (Hermes has them):** native streaming (`chat.startStream`) and edit-streaming, file uploads and downloads (attachments, images to the model), reactions as triggers, Block Kit, `reply_in_thread: false` and channel allow/ignore lists, the bot-loop guard's counters (bots answer only per `allowed_users` and `SLACK_ALLOW_BOTS`), resolving channel names (`#general`) in `send_message`, edits that add a mention, slash commands and pairing.

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

**Luau 0.740** (released 2026-09-25, MIT, commit c0e346ed), compiled to `wasm32-wasip1` with **wasi-sdk 34** (released 2026-08-25) and run by **`github.com/tetratelabs/wazero` v1.12.0** (released 2026-05-28, Apache-2.0; it requires `golang.org/x/sys`, already in go.sum at v0.47.0). wazero and `github.com/gorilla/websocket` (Slack, §4.9) are the only new Go modules in the whole design.

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

**The wasm artifact.** `internal/needletail/luau/wasm/build.sh` pins the wasi-sdk version (and each host's asset sha256 from the release), the Luau tag, its tarball's sha256 and its commit; it downloads them into a cache, builds Luau's Common, Ast, Bytecode, Compiler and VM with `src/shim.cpp`, and links `luau.wasm` (827 KB). The build is reproducible (the same bytes twice on one host; across host OSes it is expected, not yet shown). `luau.wasm` is committed and embedded with `go:embed`, and its sha256 is pinned by `TestModuleSHA256`.

**Error handling (the M6 spike).** Luau keeps its C++ exceptions (`LUA_USE_LONGJMP=0`), compiled to the standard WebAssembly exception-handling proposal (exnref: `-fwasm-exceptions -mllvm -wasm-use-legacy-eh=false`, wasi-sdk's exception-enabled libc++ and `-lunwind`), which wazero runs behind `experimental.CoreFeaturesExceptionHandling`. Every error is raised and caught inside the module. **The engine is wazero's interpreter**: its compiler (v1.12.0, the newest release) faults when Luau's parser catches its own exception (the landing pad in `Parser::parse` hands a corrupted exception pointer to the personality routine, a `call_indirect` fault in `__dynamic_cast`; a plain C++ throw through 5000 frames works), while the interpreter runs every case correctly; `TestParserErrorIsCaught` keeps it. The interpreter is about ten times slower (`fib(25)` 0.5 s, a 3-million-iteration loop 2.2 s, an instance about 5 ms) and needs no executable memory. Moving to the compiler waits for a wazero fix.

### 5.2 Sandbox

**Host side** (`luau.Engine`, behind the `luau.Runner` interface: `Check(src)` and `Run(Request)`)
- One `wazero.Runtime` per process (the interpreter, §5.1), configured with `WithMemoryLimitPages(1024)` (64 MiB) and `WithCloseOnContextDone(true)`.
- The module is compiled once. Each run gets a **fresh instance**, closed half a second after its timeout, whatever it runs: `limits.luau_timeout` for a tool the model calls (and `luau_test`), `limits.script_timeout` for a schedule's script, each 30 s by default; a header's `timeout` may only lower it.
- WASI has no preopens, no env, no args and a real monotonic clock (the wall clock is wazero's fixed fake: `os.time()` asks the host). stdout and stderr are each capped at 64 KiB.
- A `Request` names the source, the bindings it gets (§5.3), JSON args and an **entry**: empty calls the function the chunk returns (a tool), a name calls that field of the table the chunk returns, else the global function the script defined (event handlers such as `on_message`, §5.5), with the JSON argument, and returns its JSON result. A chunk that returns no function returns its result.
- A run fails with a code: `syntax`, `runtime`, `timeout`, `budget`, `memory`, `entry`, `too_large`, `cancelled` or `trap` (the module stopped).

**C shim** (`shim.c`)
- Only base, string, table, math, utf8, bit32 and buffer are opened (Luau 0.740 has no `loadstring`, `load` or `collectgarbage` in the VM).
- Removed: `getfenv`, `setfenv`, `newproxy`, `gcinfo`, `debug`, `require`, `io`, and `os` but for `os.clock` (the monotonic clock) and `os.time` (the host's clock; with a date table, Luau's own). `collectgarbage` is the shim's and accepts only `"count"`.
- `nt` and its tables are built from the bindings the host passes, frozen, then `luaL_sandbox` is applied, then `luaL_sandboxthread` for the run.
- The interrupt callback counts an instruction budget and every 256 safepoints compares the monotonic clock with the run's deadline; once either is passed every later safepoint raises again, so `pcall` cannot outlast it. The instance closing at the deadline is the backstop for code that reaches no safepoint.
- A counting `lua_Alloc` enforces a soft cap (48 MiB by default), which raises a catchable "not enough memory".
- `LUAI_MAXCCALLS` is 200 and the parser's recursion limit is lowered from 1000 to 200 (each level is several wasm frames; the interpreter allows 2000). The 1 MiB stack is placed first, so an overflow traps instead of overwriting data.
- Only source is accepted, compiled in the VM with `luau_compile` at O1; source holding a NUL byte is refused before the compiler. Agent-supplied bytecode never loads.

**Host ABI.** Two imports. `env.host_call(name_ptr,name_len,in_ptr,in_len) -> handle`: the binding's name (at most 64 bytes) and its arguments as a JSON object (at most 8 MiB); the handle is 1, or negative when the host refuses the call outright (no run, too large, a bad name, over the run's 10 000 calls). `env.host_result(handle,out_ptr,cap) -> size`: the pending answer's size, copied and released only when it fits in `cap` (the shim asks with `cap` 0, then fetches). One answer is pending at a time. Both directions are JSON:
- success: `{"ok":true,"v":…}`
- failure: `{"ok":false,"err":{"code","msg"}}` (an answer over 8 MiB becomes `too_large`)

Errors are raised as Luau errors (`"<code>: <msg>"`) on the C side, so nothing unwinds through Go; a host function that panics or answers something that is not JSON is a refusal. Module exports: `nt_alloc`, `nt_free`, `nt_out_ptr`, `nt_out_len`, `nt_check(src,len)` and `nt_run(src,len,spec,spec_len,mem_cap,timeout_ms,budget)`, where the spec is `{name, entry, args, bindings:[{name, global, params, spread, local}]}`.

### 5.3 Bindings: the frozen global `nt` and its global aliases

**One registry** (`luau.Bindings()`) defines every function a script gets: its name under `nt` (the dotted path, also the `host_call` name), its global alias or none, its parameters (sent to the host as an object keyed by their names; `spread` bindings take one table that is the whole object), its result, doc, required grant and an example, whether it runs inside the module (`local`: the JSON codec) and whether it writes or reaches out (refused in a dry run). The registry drives the module's `nt` table and globals (the shim builds them from the list the host passes), the host's dispatch, and `luau.Reference()`, a markdown reference (function, arguments, result, grant, example; then the events of §5.5) that `needletail luau reference` prints and the `luau_reference` tool returns, and `luau.CompactReference()`, one line per function, which the system prompt's guide carries (§4.4). `TestGlobalsGolden` pins what a script sees in `internal/needletail/luau/testdata/globals.json`: each binding's wire form (its name, global, parameters in order with their types, spread, grant, local, write) and the whole set of globals and `nt` keys, so any change to them is a reviewed contract change. The globals are frozen by `TestGlobals`: `read_file`, `write_file`, `append_file`, `list_files`, `stat_file`, `remove_file`, `lock`, `unlock`, `http_request`, `core_call`, `read_memory`, `search_memory`, `save_memory`, `update_memory`, `delete_memory`, `send_message`, `json_encode`, `json_decode`, `log`, `now`.

| Binding (global) | Rules |
|---|---|
| `nt.fs.read/write/append/list/stat/remove` (`read_file`, `write_file`, `append_file`, `list_files`, `stat_file`, `remove_file`) | A path is relative to the home, `~/…`, absolute under the home, or `/shared/<group\|unit>/…` (the mount's own path works too). It is lexically cleaned and never leaves its root (`..` past it is refused), then every existing component is checked with `Lstat` and symlinks are refused. The call itself reaches the file by descriptor, so a change racing the check cannot redirect it: the root (the home or the layer) is opened, then each component relative to the one before with `O_NOFOLLOW` (missing directories made with `mkdirat` for a write), and the file is opened, stat'ed, removed or listed relative to its parent's descriptor; a symlink met anywhere below the root is refused (`symlink`), in or out of the root (`os.Root` was not enough: it follows a symlink that stays in the root, and creates a dangling one's target). Only regular files are read or written; a write opens the file without truncating it and truncates it once it is known to be one. `list` reads the directory in batches of 128 up to its 1000 entries and stops when the run ends. **Protected paths** are never written (nor appended to or removed): `needletail.yaml`, `SOUL.md`, `sessions/**`, `logs/**`, `state/**`, `memories/**` (the memory tool), `skills/**` (skill_manage checks every change), `cron/**` (cronjob_manage), `scripts/**` (the job scripts schedules run: the baseline's, and the ones the agent writes with its own file tools or terminal and names in `cronjob_manage`), every hidden entry of the home's root and what is below it (`.bash_profile`, `.profile`, `.bashrc`, `.config/**`, `.local/**` with Python's user site, `.gitconfig`, `.ssh/**`, `.needletail-tmp/**`, `.swarm/**`: files programs run or obey without being told to), a Python module or package at the home's root (`*.py`, `*.pyc`, `<dir>/__init__.py`: the home is the terminal's cwd, which `python3 -c` puts first on `sys.path`, so `json.py` there would run in place of the standard `json`) and any `.git` directory (hooks, config). Names compare case-folded (`Skills/` is `skills/` on a case-insensitive home); `fs.write:home/<glob>` grants match case-sensitively, which only ever refuses more. A Luau tool can therefore never make the runtime execute something: what it writes is data the agent may still choose to run (a script it later runs by name, a `Makefile` it builds, a module a script below the root imports from its own directory), so grant `fs.write:home/<dir>/**` rather than `fs.write:home` where a tool needs one directory. In `/shared/group` and `/shared/<unit>`: `read` reads the mount; `write`/`append` go through the core's `fs_write` (mode overwrite or append, recorded as the agent's), never the mount; `list`/`stat` answer with the core's `fs_ls`/`fs_info` (the mount's own listing when off a core); `remove` is refused. Quotas per run: 8 MiB read, 4 MiB written; a read is refused when the file's size is over what is left, never reads past it (a file growing meanwhile) and is charged the bytes it read. |
| `nt.fs.lock/unlock` (`lock`, `unlock`) | The core's `fs_lock`/`fs_unlock` on a shared path; needs `fs.write:shared`. |
| `nt.http.request{method,url,headers,body,timeout_ms}` (`http_request`) | HTTPS only, no credentials in the URL. Hosts are compared normalized (lower case, no trailing dot, an IP in its canonical form) and ports as numbers (decimal, 1 to 65535, so `:0443` is 443; anything else is `bad_args`), the URL is rewritten in that form before anything dials it, and every check uses it: the grants, the core's addresses and the `http_secrets` host. The host must match the effective caps (exact or `*.suffix`). The dialer resolves once and dials the address it checked, refusing loopback, RFC 1918, CGNAT, link-local, ULA, multicast, reserved, documentation and IPv4-mapped forms of them, unless the tool names that exact IP and it is granted by that IP (`http:10.0.0.5`, never through a wildcard); the metadata addresses (`169.254.169.254`, `fd00:ec2::254`) and the core's and relay's addresses (the MCP and ingest URLs, `SWARM_CORE_URL`) are refused even then (the core is reached through `nt.core`). Proxy env is ignored. Redirects are followed by the runtime, at most 3, each URL checked again; `Authorization`, `Cookie` and `Proxy-Authorization` the script set are dropped when a redirect leaves the host. Request body at most 4 MiB, response 5 MiB, call 20 s. Answers `{status, headers, body}`. |
| `nt.core.call(tool, args)` (`core_call`) | Calls a core MCP tool as the agent, within `core:<tool>` (`core:msg_*` is granted by default on a core). An object answer comes back as it is, other text as `{result}`, a tool error as `core`. An `fs_write` to `/shared` that succeeded is attributed like `nt.fs.write`'s (a refused one is not). |
| `nt.memory.read()` (`read_memory`) | `{memory, user}` entries; read-only; needs `fs.read:home`. |
| `nt.memory.search(query, opts?)` (`search_memory`) | The archive as `memory_search` finds it (`opts`: `ids`, `kind`, `tags`, `scope`, `match`, `since`, `include_expired`, `limit`); needs `memory:read`, which `fs.read:home` does not give (nor `memory:read` `read_memory`): the archive holds more client data than the prompt. |
| `nt.memory.save(text, opts?)` (`save_memory`), `nt.memory.update(id, fields)` (`update_memory`), `nt.memory.delete(id)` (`delete_memory`) | The archive by id, as `memory_manage` (`opts`: `kind`, `tags`, `importance`, `expires`, `force`; `fields`: `text`, `kind`, `tags`, `importance`, `expires`); need `memory:write` and write (refused in a dry run). A script never puts text into the system prompt: `hot` and any pin are refused, and a hot record (the model's) is never changed or deleted (`refused`). What it saves is `src: luau:<tool>`, which recall labels "saved by a tool". Secret values the runtime knows (and the `http_secrets` values) are refused; without `memory:read`, a `similar` answer gives the records' ids, not their texts. An unknown id is `not_found`. |
| `nt.message.send(target, text)` (`send_message`) | Sends a message (Markdown) through the runtime's one messaging policy (§4.9), as the `send_message` tool does, never broadcasting: `target` in its grammar (`slack` or `home`, `slack:<channel>[:<thread_ts>]`, `slack:<user>`), within the tool's `message:<target>` caps. A target the policy refuses fails with `refused`; with no gateway running, `unavailable`; a dry run (`luau_test`) never sends. Answers `{platform, chat_id, message_id, home}`. |
| `nt.json.encode/decode` (`json_encode`, `json_decode`), `nt.json.null` | In the module, bounded (64 levels, 16 MiB): a table with keys 1..n is an array, any other non-empty table an object, `{}` the empty table; JSON `null` decodes to `nil`, `nt.json.null` encodes as `null`. |
| `nt.log(level, msg)` (`log`), `nt.time.now()` (`now`), `nt.args` | The run's log (redacted, shown to the agent and in the runtime's log, 64 KiB: past it one truncation notice in both, then nothing more); Unix time with fractions; the call's arguments. |
| **No** `nt.env` and **no** `nt.secret` | Secrets reach HTTP only through `http_secrets` grants: host → env name → header template, injected by Go. A script is never given the value; what is guaranteed is masking: every answer a script gets, its results, output and logs have the runtime's secrets masked (values of 8 bytes or more, as they are and as Go's JSON writes them) and the `http_secrets` values masked besides, whatever their length, in any spelling a JSON string allows (each character as itself or `\uXXXX` in either hex case, surrogate pairs, `\/`, `\"` and the other short escapes), in every string of an answer before the host encodes it and in the encoded answer after, so `json_decode` of an echoed header or body cannot recover one. A server that echoes a credential encoded some other way (base64, percent-encoding, split across fields) can still hand it to the script, which may then write or send it wherever its caps allow: grant `http_secrets` only for hosts that do not echo credentials. |
| **No** exec | There is no process capability. |

**Audit**
- Every host call is appended to `logs/audit/<session>.jsonl` (Raw, mode 0600) as `{ts, tool, call, cap, args_sha256, bytes_in, bytes_out, status}` (`call` the binding, `cap` its grant, `status` ok or the refusal's code, such as `denied`).
- The outer tool call emits `tool_start` and `tool_done` hooks named `luau:<tool>` (the bench's stream names it so too); `tool_done`'s input carries `path` (and `paths`) for the `/shared` files the run wrote through the core, so the core attributes them.

### 5.4 Tool format (contract v1)

A tool lives at `skills/<cat>/<skill>/tools/<name>.luau` (`name`: a lowercase letter, then lowercase letters, digits or `_`, at most 48). It starts with a YAML header (at most 16 KiB, unknown keys refused; closed by a line that is exactly `]]`, and holding no `]]` before it, since Luau ends the comment at the first one and would run the rest as code; `name` must be the file's; `description` at most 1024 characters; `args` an object schema, default an empty one; `timeout` at most 10 min) and its body returns a function:

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
| `memory:read`, `memory:write` | The memory archive: `search_memory`; `save_memory`, `update_memory`, `delete_memory`. Neither implies the other, nor anything of `fs.*`. |
| `message:<target>` | `send_message` to a target in its grammar: `message:slack` the home channel, `message:slack:<channel>`, `message:slack:<channel>:<thread_ts>`, `message:slack:<user>`; a `<prefix>:*` form covers the prefix and everything under it (`message:slack:<channel>:*` a channel and its threads, `message:slack:*` every Slack target), `message:*` any. The messaging policy still applies within it. |

**Effective caps = declared ∩ granted.**
- Grants live only in `engine/needletail/config.yaml` (rendered into `needletail.yaml`). Both files sit outside `skills/` and are not writable by the agent, so a tool, or a dream-proposed tool, cannot grant itself anything.
- Default grants: `fs.read:home`, `fs.write:home` (excluding protected paths) and `core:msg_*` when on core.
- `http`, `fs.*:shared` and `message` are off unless granted.
- A call outside the effective caps fails with code `denied` and is audited.

**Lifecycle**
- `skill_manage` validates the header and runs `luau check` before saving. A check is kept per content: a syntax error as long as the file is unchanged, a failure of the engine itself (the check timed out) with its reason for 10 minutes, so a file that makes the check fail is not checked again every turn; a check the turn cancelled is not kept. The compiler's own failures say why (`the compiler ran out of memory`, or the exception's message).
- At turn start, valid tools are registered as `<skill>__<name>` (the skill dir's last segment; at most 64 characters), replacing the last turn's set, so a tool written in one turn is offered from the next and a removed or broken one stops being offered. Invalid ones (and one whose name another tool has) are listed with their error in `skill_view` and never registered. Symlinked files are ignored.
- Because tools live under `skills/[!.]*/**` (Learning), the dream's existing skill diff proposes them with no dream change. The proposal review should show the declared caps.

### 5.5 Event handlers (contract v1, M6b)

The agent manages its own interactions with handlers: Luau that runs before, or instead of, a model turn when something happens, with the same sandbox, caps and audit as a tool.

**Layout.** `skills/<cat>/<skill>/handlers/<name>.luau`, beside the skill's `tools/`. A separate directory rather than a header key on tools: a handler is never offered to the model (Discover reads only `tools/`, so a handler cannot become a tool by mistake), its argument is the event the runtime builds, not a schema the model fills, and its entry is a function named after each event rather than the function the chunk returns. Handlers are Learning like the rest of a skill (`skills/[!.]*/**`, pinned in `testdata/classify.json`): the nap keeps them, the dream proposes them with their skill, and review sees their caps and events.

**Header.** The tool header (§5.4) with `events: [on_message, on_mention, on_schedule, before_send]` (at least one) and, with `on_schedule`, `jobs: [<job id or name>, …]` (or `["*"]` for every job; at most 64), and no `args`; `name` (the file's) and `description` are required. A tool's or a script's header with `events` or `jobs` is refused, and so is a handler's with `args`. `skill_manage` checks a handler like a tool (header, then a compile) on the whole file it would leave; `needletail luau check` does too.

```lua
--[[needletail
format: 1
name: hours
description: Answers opening hours without a turn.
events: [on_message, on_mention]
caps: [message:slack]
]]
function on_message(ev)
  if string.find(string.lower(ev.text), "hours") then
    return {action = "reply", text = "We open at 9."}
  end
  return nil -- pass: the model answers
end
on_mention = on_message
```

**Calling convention.** For each event it handles, the file defines a function of the event's name, a global or a field of the table the chunk returns (`Request.Entry`, §5.2), called with the event as JSON; it returns a decision: `nil` (the event's default), the action as a string, or `{action, text, context, reason}`. Each event's actions, the first being the default:

| Event | When | Event fields | Actions |
|---|---|---|---|
| `on_message` | a Slack message the gateway answers (a DM, or a reply in a thread the agent is in), before a turn is taken | `platform, session, channel, thread_ts, ts, dm, mentioned, reply, user, user_name, bot, verified, text, thread_context[] {user, name, text, assistant, parent, unverified}` | `pass` (`context`: a note for the model under the message, at most 4 KiB), `reply` (`text`, at most 40 000 characters: posted in the conversation, no turn), `silent` (nothing posted; a DM or a mention follows the `[SILENT]` rule: nothing posted, logged) |
| `on_mention` | the same, for a message that mentions the agent (instead of `on_message`: one event per message) | as `on_message` | as `on_message` |
| `on_schedule` | an agent job's run (not a script-only one), after its monitor and pre-run script, before its turn; only the handlers whose `jobs:` name the job | `job {id, name, schedule, prompt, skills, deliver, repo}, script_output, run_context, time` | `pass` (`context`: a "Handler Notes" block in the run's prompt), `reply` (`text`: the run's output, delivered as a turn's would be, no turn; the monitor's change counts as handled), `silent` (a silent run) |
| `before_send` | every message the gateway posts (`kind`: `reply`, `handler_reply`, `send`, `delivery`, `notice`), redacted, before formatting | `platform, target, channel, thread_ts, kind, session, text, broadcast` | `allow`, `rewrite` (`text`: the text only), `block` (`reason`) |

**Order and failures.** An event's handlers run in a fixed order: by skill dir, then file name (each compared as bytes: all of `ops/a`'s before `ops/a-b`'s, which the whole path would put first, `-` being before `/`). A message's or a schedule's run until one answers `reply` or `silent`; each `pass` adds its context (at most four kept) and the next runs. `before_send`'s run in order too, each seeing the text the previous ones left: a `rewrite` changes it and goes on, a `block` stops the message. A message's or a schedule's handler that fails (a Luau error, a refused call it did not catch, no function for the event), passes its bound or answers something that is not one of the event's actions is skipped: logged (`handler <skill>__<name> (<event>) skipped: …`), hooked as failed, and the event goes on as if it had passed: failing open keeps a broken handler from silencing the agent. **`before_send` fails closed** (the owner's decision): such a handler, or a file in a skill's `handlers/` that cannot be loaded (it does not compile, its name is not the file's, a cap, timeout or key its header may not have: one the terminal wrote, since `skill_manage` refuses it) when its header declares `before_send` (read leniently: the events line is taken from a header that otherwise fails) or names no events that can be read (no header, unreadable, over 1 MiB, or not a regular file: any entry named `*.luau` there, a symlink, a FIFO or a directory, counts), blocks the message, which is logged (`handler <skill>__<name> (before_send) failed: the message is blocked …`) and, when it ran, hooked as failed. A handler there is a filter (a rewrite that removes what must not go out), so a failure must not let the text it was meant to change through; a rewrite over 40,000 characters, for one, is not a valid decision and blocks. What a block means is the same as a handler's own `block` (a turn's or a handler's reply is not posted, logged, a turn's 👀 replaced by ❌; `send_message` (and Luau's) returns an error, `blocked`; a job's delivery is `delivery_failed`), but for the runtime's own notices: its busy and error notes and a job's failure or auto-pause notice are posted all the same (redacted, a delivery still under the messaging policy; logged), so a broken filter cannot hide that something failed (a handler's deliberate `block` still holds them). A job's failure notice can carry what the run said (a `[CRON_FAILURE]` reason, a script's error output, at most 16 KB). The next handlers do not run. Fix the handler (`skill_manage`, `luau_test`) or remove it to let messages out again. Every run is hooked as a tool call `luau:<skill>__<name>#<event>` (`tool_start`, `tool_done` with the action or the failure), audited like a tool's calls (`logs/audit/<session>.jsonl`).

**Bounds and caps.** A handler runs with its effective caps (declared ∩ granted, §5.4) and the Luau bound: `limits.luau_timeout` for message and send events (they hold a conversation), `limits.script_timeout` for `on_schedule` (part of a scheduled run, like a `.luau` script); a header's `timeout` may only lower it. A `reply` needs no `message:` cap: it answers the conversation it was written to, as the model's reply does.

**No recursion.** The event runs in the handler's context. `send_message` from an `on_message`, `on_mention` or `on_schedule` handler (within its `message:` caps) goes through `before_send` once, then the messaging policy. In a `before_send` handler `send_message` is refused (`refused`): it decides on one message and cannot start another, so `before_send` never runs inside itself; a handler's own `reply` goes through `before_send` like any message. A handler can never widen where a message goes or add a broadcast: `before_send` changes only text, the policy decides the target after it, and Luau never broadcasts.

**`luau_test`** dry-runs a handler with a sample event (`luau.SampleEvent`) and the fields given, and shows the decision the runtime would act on (§4.5).

**Not built.** A `route` action (to another agent or conversation), handlers for the API's or the core's messages, reactions or edits as events. A job names its handler only from the handler's side (`jobs:`): `cron/jobs.json` and agent.yaml (engine-neutral) are unchanged, so a job works the same on Hermes and a promoted job keeps its handler through its skill.

## 6. Storage and mounts

**Home.** `Layout.Home = /opt/data`, the same path as `AGENT_HOME`, on the task-local volume. Rehydrate (root) hands it all to `Layout.RuntimeUID` (10000) as for any engine; the runtime, started as root, then lays it out for the uid split before anything writes (`privsep.Prepare`, by descriptor, never following a symlink; §7):

| Entries | Owner | Mode | Why |
|---|---|---|---|
| the home itself | 10000:10000 | 3775 | the agent's commands may make files and directories there (git clones, work files) but, sticky, never remove or rename the runtime's entries |
| `.tmp/` (the commands' `TMPDIR`) | 10000:10000 | 2770; what it holds is the workspace's (below) | the agent's commands' temporary files, since `/tmp` is outside the jail and, on a read-only root, not writable. The runtime's, so a command can neither remove it nor swap it for a symlink (to `/tmp`, say: the home is sticky); a non-directory holding the name is moved aside. No snapshot rule claims it, so naps never keep it; it is not emptied at boot (task-local on ECS, kept in a local home) |
| `cron/`, `state/` (and `state/needletail/`), `sessions/`, `logs/` (and `logs/audit/`), `.swarm/`, `.needletail-tmp/` | 10000:10000 | 0700, files 0600 | the runtime's own; made when missing so a command cannot take the name first; a file or symlink holding the name is moved aside (`<name>.moved-<time>`, logged) |
| `memories/` | 10000:10000 | 0750, files 0640 (RECORDS.md too, as rehydrate restored it: `TestPrepare`) | changed only through the memory tools; readable by the commands (the hot files are in the system prompt anyway; RECORDS.md, the archive, is readable by the owner's decision, §11) |
| `needletail.yaml`, `SOUL.md` | 10000:10000 | 0600, 0644 | the runtime's; `SOUL.md` made empty when missing (a command could otherwise plant one); anything but a regular file fails the boot |
| everything else: `skills/`, `scripts/`, the manifest's state dir (`<state_dir>/`, `needletail.yaml`'s `state_root`), the agent's dotfiles and work files | 10001:10000 | the group gets the owner's bits (dirs g+rwx; a read-only file stays read-only; no setuid or setgid file). In `skills/`, `scripts/` and the state dir every entry gets them and at least read (dirs r-x), an owner-only one too (a `mktemp` file or directory, 0600/0700, a command moved there); elsewhere an entry with no group or other bits (0600, 0700: `~/.ssh`, `~/.gnupg` and their keys, which ssh and gpg refuse once the group may read them) keeps its mode | the agent's workspace: its commands own it (so `chmod +x` works on what rehydrate restored) and the runtime, same group, reads and writes `skills/`, `scripts/` and the state dir: what rehydrate restores there (0755/0644) and what the runtime makes (0775/0664) has group bits, and what a command made owner-only there is given them at the next boot (until then the runtime cannot reach it: `skill_manage` there fails) |

The runtime's files come last, so an inode linked from the workspace ends up the runtime's. The runtime's umask is 002 and what it makes in the workspace is 0664/0775, so the agent's commands can change it; a file the runtime made is still the runtime's, so a command cannot `chmod` it (run scripts with their interpreter). An instance whose `names.state_dir` is `state` would put the manifest's state under the runtime's `state/`, out of the commands' reach: keep the default (`<slug>-state`).

**Snapshot rules** (first match wins; golden-tested in `testdata/classify.json`)

| Class | Paths |
|---|---|
| Learning | `memories/MEMORY.md`, `memories/USER.md`, `skills/.usage.json`, `skills/[!.]*/**` (including `tools/*.luau` and `handlers/*.luau`), `cron/jobs.json`, `needletail.yaml` (napped but rebuilt from the baseline on every boot, as Hermes' `config.yaml`: a changed manifest or grant reaches the agent, and nothing the agent writes into it survives a restart) |
| Raw (PII) | `sessions/**`, `memories/RECORDS.md` (the memory archive: client data; restored verbatim at rehydrate and never read by the dream, which learns from the hot files alone, so the ledger, scrub and promotion invariants hold unchanged), `logs/audit/**`, `state/needletail/jobs/**` (schedule run records) and `state/needletail/monitor/**` (monitor outputs): what jobs fetched and replied is client data |
| State | `<StateRoot>/**`, `state/needletail/**` (idempotency keys, Slack state, `memory-usage.json`: the records' counters, ids and numbers only) |

`SOUL.md` and `scripts/**` are rebuilt from the baseline. `.needletail-tmp/` is claimed by no rule, and `serve` empties it at boot before anything writes through it (`oneshot`, which may run beside a serving runtime, does not): `skill_manage` and the usage counts write their temporary files there before renaming them into `skills/`, and so do the session header and `sessions/index.json` rewrites, `state/needletail/idem.json` and the scheduler's files (`cron/jobs.json`, monitor snapshots, run outputs) (a temporary file beside a skill file would be claimed by `skills/[!.]*/**`, so a nap taken mid-write, or a crash before the rename, would keep it as part of the skill). A deleted skill is moved there in one rename before its files are removed (`os.RemoveAll`, which never follows a symlink), so a nap never keeps half of it. What resists removal (a directory a command made read-only, or the commands' own under the uid split, which the runtime may not change) stays there as it is: the runtime never changes a mode by path in what the agent's commands can reach, since a symlink a command races in where a directory was would redirect it (to the home's root, say, or `/shared/group`). The boot sweep removes it after `privsep.Prepare` has given the whole of `.needletail-tmp/` to the runtime: by descriptor, a symlink removed as itself, a read-only directory made writable through the descriptor it was opened by (`fsx.Sweep`).

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
| `RuntimeUID/GID` | `shared.PosixUID` (= `wire.RuntimeUID`, pinned by `TestRuntimeUsers`). The exec uid (10001) is the runtime's own business and is not in the Layout (§3). |

**Mounts**
- The container's root filesystem is read-only (`Layout.ReadOnlyRoot`; compose `read_only`, ECS `readonlyRootFilesystem`): the runtime writes only to the home and `/shared`, and needs no tmpfs.
- The jail (§7) confines the agent's commands to these mounts: the home and the two shared layers, nothing else of the container.
- `/shared/group` and `/shared/<unit>` are mounted read-write exactly as today, because the bash terminal needs them. The commands (10001:10000) write there by group (the layers are 2775 with group 10000, as the EFS access points create them) and, on ECS, as 10000 anyway: an access point performs every operation as its POSIX user (10000:10000), whoever the process is (§7).
- File tools are confined by `sandbox`; Luau writes to `/shared` go through the core.
- The agent's private `data/` is never mounted.
- Secrets arrive only as container env.

## 7. Security

- **The real boundary is the container.** It runs with `no-new-privileges`, dropped capabilities and a read-only root filesystem, and its only mounts are home and its own shared directories. Inside it, the **uid split** (M6b, below) keeps what the agent executes out of the runtime's files, and the **jail** (below) keeps it inside those mounts: the bash `terminal` tool has the network, the workspace and the shared space, and reads only the system's programs besides. The Luau sandbox constrains only Luau.
- **The uid split** (`internal/needletail/privsep`). The image starts as root. `serve` and `oneshot` lay the home out (§6) by descriptor (`openat` with `O_NOFOLLOW` from the home, `fchown`/`fchmod` on what they opened, `fchownat(AT_SYMLINK_NOFOLLOW)` for a symlink, so nothing is followed), then drop, on every thread (`syscall.AllThreadsSyscall`: credentials are per thread on Linux and Go has several; the binary is built without cgo): the bounding set loses every capability but `CAP_SETUID`, `CAP_SETGID` and `CAP_KILL` (best effort, it needs `CAP_SETPCAP`; the log says when it could not), `PR_SET_KEEPCAPS`, no supplementary groups, `setresgid`/`setresuid` to 10000:10000, then `capset` keeps those three in the effective and permitted sets with nothing inheritable, the ambient set is cleared, `PR_SET_DUMPABLE` 0, and `/proc/self/task/*/status` is checked for every thread (uid, gid, groups, each capability set); a failure stops the boot. Umask 002. The terminal, schedule scripts and monitors start with `SysProcAttr.Credential{Uid: 10001, Gid: 10000, Groups: []}` in their own process group: the uid change between two non-root uids keeps the runtime's capabilities only until `execve`, which leaves a non-root process with no file capability (and `no-new-privileges` forbids any) with none, so the commands run with no capability at all. `CAP_SETUID`/`CAP_SETGID` start them so, `CAP_KILL` kills their group at a timeout (another uid's). It works under compose's hardening (`no-new-privileges`, `NET_RAW`, `MKNOD`, `SYS_CHROOT`, `SETFCAP`, `AUDIT_WRITE`, `NET_BIND_SERVICE` dropped; `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETUID`, `SETGID`, `KILL`, `SETPCAP` remain). Started as any other user (`--user`, an old manifest) the runtime runs as that one user, as before, and says so in its log; `NEEDLETAIL_UID_SPLIT=off` runs a root start without the split (development and the cli tests only; never in the image).
- **What the split holds.** A command cannot write `cron/`, `state/`, `sessions/`, `logs/`, `.swarm/`, `memories/` or `needletail.yaml`, read the first five, rename or remove any of them (sticky home), read the runtime's `/proc/1/environ` or signal it; it can work in `skills/`, `scripts/`, the home and `/shared`. The runtime's writes into the workspace (the file tools, `skill_manage`, the usage counts) reach their directory by descriptor (`fsx.*Below`), so a command racing them with a symlink swap cannot redirect one into the runtime's files; reads through a planted link reach nothing the model could not read with `read_file`. The files the runtime reads from the workspace on its own, at every event or turn (the skills index's front matter, the Luau tools and handlers, a SKILL.md or skill file `skill_view` and `skill_manage` read, a schedule's `.luau` script), are found and opened the same way (the skills index and a skill's file list walk `skills/` by descriptor, `fsx.WalkBelow`, never entering a symlink nor a directory swapped for one while listed; `fsx.ReadFileBelow`, `fsx.ReadDirBelow`, `fsx.OpenFileBelow`: every component with `O_NOFOLLOW`, the file with `O_NONBLOCK` and checked a regular file on the descriptor, at most the file's bound read), so a FIFO or a symlink to a device (`/dev/zero`) a command swaps in after a check can neither block the runtime nor feed it without end; a `.luau` script is read from the path the scheduler resolved, every component again with `O_NOFOLLOW`. The runtime itself keeps `CAP_SETUID`: code that took the runtime over could become any user (root without other capabilities) — the split protects the runtime's state from the agent's commands, not from the runtime.
- **Verified in Docker** (M6b, `needletail:dev` built from this tree, run as compose runs an agent: `docker run --security-opt no-new-privileges:true --cap-drop NET_RAW --cap-drop MKNOD --cap-drop SYS_CHROOT --cap-drop SETFCAP --cap-drop AUDIT_WRITE --cap-drop NET_BIND_SERVICE`, the home a volume filled as rehydrate fills it (all 10000:10000), `/shared/group` a 2775 volume as an access point makes it and `/shared/sales` a host bind mount, a stub model on the host): a turn through `POST /v1/sessions/{id}/chat` ran a terminal command that reported `uid=10001 gid=10000`, `CapPrm`/`CapEff`/`CapAmb` 0 and `CapBnd` `e0`, and was denied writing `cron/jobs.json`, `state/needletail/`, `needletail.yaml`, `.swarm/booted.json`, `sessions/`, `memories/MEMORY.md`, reading `cron/jobs.json` and `/proc/1/environ`, `kill -0 1`, `mv cron` and `rm needletail.yaml`, while it wrote `skills/crm/crm-api/scripts/probe.sh` (and `chmod +x` it), a `work/` directory in the home, `/shared/group` and `/shared/sales`. All 16 runtime threads showed uid and gid 10000 in every slot, no groups, `CapEff`/`CapPrm` `e0`, `CapInh`/`CapAmb` 0, `NoNewPrivs: 1`; a script-only job ran as 10001 with no capability; the health check went healthy; a restart split again; `oneshot` in a fresh container answered through the terminal the same way; `--user 10000` ran unsplit and said so. A second run (the same hardening, a scripted stub model driving real tool calls through the API) checked the review's fixes: a terminal command as 10001 ran `git status` in a `write_file`-made directory (10000:10000) and in `/shared/group` (exit 0; the same command without `GIT_CONFIG_COUNT` failed with "dubious ownership"); `skill_manage delete` of a skill holding a 10001 `0700` directory, while a command swapped directories and symlinks to `memories/` and `/shared/group` in it as fast as it could (`renameat2` exchange), left the home 3775, `memories/` 0750 and `/shared/group` 2775, left the skill in `.needletail-tmp/` with its modes untouched, and the next boot emptied it; `~/.ssh` (0700) and its key (0600) kept their modes across boots. A third run (the final review's fixes, same hardening) gave a home a command's owner-only entries (10001, a `0700` directory with a `0600` file in `skills/`, a `0700` `scripts/` file, a `0700` directory in the state dir `acme-state/`, `~/.ssh`, a `0700` `work/` directory): the image before the fix left them all `0700`/`0600`, so uid 10000 could neither read nor write them; this tree's boot made those in `skills/`, `scripts/` and `acme-state/` `0770`/`0660` (10000 read and wrote them; a restart kept it) and left `~/.ssh` and `work/private` `0700`/`0600`, still read by 10001; the health check went healthy and `TestDropAsRoot` passed as root in the hardened container. `TestDropAsRoot` (`internal/needletail/privsep`) is the same check as a test: build it with `GOOS=linux CGO_ENABLED=0 go test -c` and run the binary as root in the hardened container.
- **On ECS** (render only). The task definition sets no `user`, so the agent container starts as the image's root and splits itself; it must not set `user: 10000` (that runs unsplit). The default capabilities (EC2 and Fargate) include the ones the split needs (`CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETUID`, `SETGID`, `KILL`, `SETPCAP`); a `linuxParameters.capabilities.drop` mirroring compose's list is a neutral follow-up (§3), and `no-new-privileges` (`dockerSecurityOptions`) exists on EC2 only: on Fargate a command could gain privileges only through a setuid or file-capability binary, the image has none (checked: no setuid or setgid file in `needletail:dev`), and a command cannot make one (it cannot give a file to root); keep it so when the image changes. `initProcessEnabled` would make an init PID 1 (root's `/proc/1`); the runtime is then not PID 1 and is still another uid's, non-dumpable. The EFS access points stay as they are (`--posix-user Uid=10000,Gid=10000`, root directory 2775): an access point performs every NFS operation as its POSIX user, so a command's writes to `/shared` land as 10000:10000 like the runtime's, and the layer's isolation is still the task role's access points. The home volume is task-local and filled by rehydrate as root, as before. The agent container renders `readonlyRootFilesystem: true` (`Layout.ReadOnlyRoot`; supported on Fargate, unlike `dockerSecurityOptions`) and no tmpfs: the runtime writes only to the home and `/shared`.
- **The jail** (`internal/needletail/jail`). The uid split keeps the agent's commands out of the runtime's files, not out of the rest of the container: without more, they could read the whole image and write `/tmp`. The jail confines them with Landlock to the directories mounted for the agent, like any other worker. Go cannot run code in a child between fork and exec, so every command the runtime starts (the terminal, schedule scripts, monitors: `tools.Command`) is started through the runtime's own binary (found with `os.Executable` before the split): `needletail jail --home <home> --shared /shared/group --shared /shared/<unit> -- <program> <argv…>`, a hidden subcommand that runs with the command's user, directory, environment and process group. It locks itself to its OS thread (Landlock and `no_new_privs` are per thread, and `execve` keeps only the calling one), makes `<home>/.tmp` if a command removed it, builds a ruleset, sets `PR_SET_NO_NEW_PRIVS`, calls `landlock_restrict_self` and execs the program. Everything the command starts inherits the domain, which nothing can lift. A launcher that cannot set the jail up exits 125 having run nothing; one that cannot exec the program exits 126. It costs about 5 ms a command. The rules (`jail.Spec.Rules`; a path missing from the image is skipped, the home is required):

  | Paths | Rights |
  |---|---|
  | the home, `/shared/group`, `/shared/<unit>` (those mounted) | everything but making device nodes: read, write, make and remove files, directories, symlinks, FIFOs and sockets, execute; renaming across directories (`refer`, ABI 2), truncating (ABI 3), ioctl on devices (ABI 5) |
  | `/bin`, `/sbin`, `/lib`, `/lib64`, `/usr/{bin,sbin,lib,lib64,libexec,share}`, `/usr/local/{bin,sbin,lib,libexec,share}` | read and execute. Landlock cannot grant execute alone (`execve` opens the file for reading), so the runtime's binary in `/usr/local/bin` is readable; it holds no secret |
  | `/etc/ssl`, `/etc/ca-certificates` and the files `/etc/{passwd,group,hosts,resolv.conf,nsswitch.conf,host.conf,services,protocols,hostname,localtime,os-release,alpine-release,mime.types,gitconfig,ld.so.cache,ld-musl-*.path}` | read: users, DNS, service names, TLS roots, git's and python's system files |
  | `/proc` | read. A rule on `/proc/self` would bind the launcher's own pid, not its children's. The runtime's entries stay root's (non-dumpable) and another uid's, so `/proc/1/environ` stays out of reach |
  | `/dev/{null,zero,full,random,urandom,tty}` | read, write, truncate, ioctl |
  | everything else: `/`, `/tmp`, `/var/tmp`, `/root`, `/opt`, `/app`, `/dev/shm`, `/sys`, the rest of `/etc` (`/etc/shadow`, `/etc/apk`), another unit's layer even if it were mounted | nothing: no read, write or execute |

  Landlock only takes rights away: inside the home the split's modes still keep `cron/`, `state/` and the rest out of reach. `TMPDIR`, `TMP` and `TEMP` are `<home>/.tmp` (§6), for the commands whenever the split is on, jailed or not.
  **ABI.** The launcher asks the kernel for its Landlock ABI (`landlock_create_ruleset` with `LANDLOCK_CREATE_RULESET_VERSION`) and handles every filesystem right that ABI knows: ABI 1's thirteen, `refer` from 2, `truncate` from 3, `ioctl_dev` from 5. Every rule is masked to the handled rights, and a rule on a file keeps only a file's rights (any other is `EINVAL`). On ABI 1 the kernel refuses every rename or link across directories (`EXDEV`; `mv` then copies and removes). Rights a later ABI adds are not handled, so they are not restricted (best effort). The jail is filesystem only: the network rights (ABI 4) and scopes (ABI 6) are not handled, so the network is as before.
  **When.** With the uid split on (the image started as root on Linux) and Landlock in the kernel. `NEEDLETAIL_JAIL=off` turns it off (development only). A runtime started as another user (`--user 10000`) runs unjailed, as it runs unsplit.
  **Fallback.** A kernel without Landlock (before 5.13: `ENOSYS`; built without it or booted without it in its LSM list: `EOPNOTSUPP`; a container cannot read `/sys/kernel/security/lsm`, so the syscall is the test) leaves the agent working as before. The boot logs `JAIL OFF: …` once, loudly, and `GET /v1/activity` reports `jail: "off: <why>"` (`"landlock ABI <n>"` when on). The adapter carries it to the core as `engine.Activity.Jail`, where the office and `stormo review` can show it. Unjailed, the commands keep the uid split, the read-only root (nothing of the image and no `/tmp` is writable) and the container's mounts (another unit's layer is never mounted), but they can read the whole image.
  **Fargate.** AWS does not publish the kernel of a Fargate platform version. Its changelog says only that Linux platform version 1.4.0 (the latest) is "Based on Amazon Linux 2" ([platform version changelog](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/platform-versions-changelog.html)). Amazon Linux 2's kernels are 4.14, 5.4 and 5.10 (5.15 as an extra), and Landlock needs 5.13 or later, built with `CONFIG_SECURITY_LANDLOCK` and enabled in the LSM list ([kernel docs](https://docs.kernel.org/userspace-api/landlock.html)). Expect the fallback on Fargate until a task says otherwise: the first deployed task's activity `jail`, or `uname -r` through ECS Exec, settles it. On ECS on EC2 with an AL2023 AMI (kernel 6.1 and later) Landlock is expected; check the same way. The read-only root holds on both.
  **Not covered.** `/dev/shm` is outside the jail, so Python's `multiprocessing` locks fail (threads work). `.tmp/` is not emptied at boot.
- **Verified in Docker** (the jail; `needletail:dev` from this tree, OrbStack's kernel 7.2, Landlock ABI 10, run as compose runs an agent with the same hardening plus `--read-only`, the home a volume filled as rehydrate fills it, `/shared/group` a 2775 volume, `/shared/sales` a host bind mount, `/shared/other` a world-writable volume holding a world-readable file, mounted but not the agent's unit, and `/app` a volume with a world-readable file; a stub model on the host turned each chat message into one terminal call through `POST /v1/sessions/{id}/chat`). The boot logged the jail on and `/v1/activity` reported `"jail":"landlock ABI 10"`. A terminal command ran as 10001:10000 with `CapEff` 0 and `NoNewPrivs` 1 and `TMPDIR=/opt/data/.tmp`, and **could**: write `work/` in the home, `mv` across its directories, truncate a file, write `skills/`, write `/shared/group` and `/shared/sales`, `mktemp` (in `.tmp/`), run bash, python3 (`tempfile`, `ssl.create_default_context()`, `getaddrinfo("localhost", "https")`), `git init` and commit in the home, `git status` in `/shared/group`, read the TLS roots, `/etc/resolv.conf` and `/etc/hosts`, use process substitution, run a skill's script with bash and a `chmod +x` script of its own, and read `/proc/self/status`. It **could not**: read `/etc/shadow`, `/etc/shells` or `/etc/apk/world`, list `/`, `/root`, `/opt` or `/tmp`, read `/app/config.txt`, read or write `/shared/other` or list it, write `/tmp`, `/var/tmp`, `/usr` or `/dev/shm`, read `cron/jobs.json` (the split) or `/proc/1/environ`. A script-only job's script ran jailed the same way (denied `/tmp`, `/etc/shells` and `/shared/other`; wrote `/shared/group`; python's `tempfile` worked). The health check went healthy; `oneshot` in a fresh read-only container answered through a jailed terminal the same way. With a seccomp profile making the Landlock calls fail with `ENOSYS` (a kernel before 5.13), the boot logged `JAIL OFF: the kernel has no Landlock (before 5.13)…`, the activity reported `"jail":"off: the kernel has no Landlock (before 5.13)"`, and the agent ran as before: commands read `/`, `/etc/shells`, `/app` and the mounted `/shared/other`, while the read-only root still refused `/tmp` and `/var/tmp` and the split `cron/` and `/proc/1/environ`. `TestJailAsRoot` (`internal/needletail/jail`) is the jail's check as a test, and `TestDropAsRoot` still passes: build each with `GOOS=linux CGO_ENABLED=0 go test -c` and run the binary as root in the hardened container.
- **Luau:** two layers (wasm and the Luau sandbox), capabilities granted out of band, no env, no secrets, no exec, protected paths, symlink refusal, quotas, an SSRF dialer that pins the IP, and an audit log.
- **No execution by planted file.** Nothing the runtime starts reads a file a Luau tool can write: the terminal is not a login shell and reads no rc file (§4.5), schedules run only `scripts/**`, and Luau never writes `scripts/**`, `skills/**`, the home's hidden entries or a `.git` (§5.3). The file tools (`write_file`, `patch`) still write `scripts/` and the home's dotfiles: they are the model's own tools beside the terminal, which reaches all of it anyway, and they are how the agent writes a job's script before naming it in `cronjob_manage` (M4); with no startup file read, a dotfile they write runs nothing by itself. Everything those run runs as the agent's user (the split), never as the runtime.
- **Messages:** every message the agent sends on its own (`send_message`, Luau's `send_message`, the deliveries of the jobs it made) goes through one policy (§4.9): the home channel, the conversations it is in, and the DMs of the people `allowed_users` names; a broadcast only with the call's `broadcast: true` and the `slack.broadcast` grant, every `<!…>` token in model text escaped otherwise. Slack answers only the people and bots `allowed_users` lists; thread context tags everyone else `[unverified]`.
- **The managing tools' state:** `cron/**`, `.swarm/**`, `state/**`, `logs/**`, `sessions/**`, `memories/**`, `SOUL.md` and `needletail.yaml` are never written by the file tools (§4.5) nor by Luau (§5.3), and with the uid split not by the terminal or a script either, so `cronjob_manage`'s and `skill_manage`'s limits cannot be skipped (on Hermes, and with the split off, a determined agent can still edit `cron/jobs.json` or `.swarm/booted.json` from the terminal).
- **Handlers** (§5.5) are agent-written Luau with the same caps and audit as tools; they see only messages the gateway answers (after `allowed_users`), never widen a target or add a broadcast (the policy decides after `before_send`), never recurse (no send from `before_send`). A broken message or schedule handler is skipped (logged and hooked: fail open); a broken `before_send` one blocks the message (fail closed), so a filter that fails never lets the text it was there to change out.
- **Secrets:** read into a struct and unset from the env except the passthrough allowlist; the process is non-dumpable and, with the split, another uid's, so its start-up env in `/proc` is not readable by the terminal; the terminal gets an explicit env without them; redacted from outputs, logs and hooks; injected into HTTP headers only by Go.
- **API:** bearer key on everything except `/health`. Idempotent runs and a busy cap.
- **Hooks:** HMAC-signed and timestamped (the core rejects anything more than ±10 min off). Unique delivery ids.
- **Supply chain:** two new modules (gorilla/websocket v1.5.3 and wazero v1.12.0, each at least 2 weeks old). Base images pinned by digest. The `luau.wasm` sha256 is pinned and its build is reproducible from `build.sh`.
- **Adversarial test suite (M6)** (`internal/needletail/luau`: `engine_test.go` for the VM, `host_test.go` for the bindings), which fails closed:
  - infinite loop, killed at the deadline
  - memory bomb, which gets an out-of-memory error
  - `../` and symlink escape
  - writes to protected paths
  - SSRF to `169.254.169.254`, RFC1918 and the core
  - redirects to a disallowed host
  - an undeclared or ungranted cap
  - no env visible
  - agent-supplied bytecode refused
  - also: removed globals and frozen libraries (`getfenv`, `load`, `string.dump`, `debug`, `io`, `os.getenv`, writes to `nt` or `string`), C-stack recursion through metamethods, `gsub` and `sort`, huge output, malformed, panicking and oversized host answers, unknown bindings, too many host calls, oversized arguments, DNS answers that mix in an internal address, the core's own port, proxies in the env, secrets echoed back by a server (also JSON-escaped, and short ones), the core or a file

## 8. Versioned contracts

Each is frozen at the first nap of a needletail agent and covered by golden tests in `pkg/engine/needletail/testdata`:
- the memory separator `\n§\n`
- `memories/RECORDS.md` v1: entries joined by `\n§\n`; the first is the version line `<!-- needletail-records v1 -->`; each record is one header line `<!-- {"id","kind","tags","imp","hot","src","created","updated"[,"expires"][,"pii"]} -->` (keys in this order, strings escaped as `encoding/json` writes them, so `-->` cannot end the comment; keys a runtime does not know are kept, sorted, after its own) followed by the text; the file ends with a newline. A v2 ships with a migration in the store's load; an older runtime never writes a newer file
- `state/needletail/memory-usage.json` (`{"v":1,"records":{"<id>":{uses,last_used,recalls,last_recalled}}}`)
- the `skills/.usage.json` shape (Hermes' keys, §4.5)
- the `cron/jobs.json` shape (shared with Hermes)
- session JSONL v1
- hook body v1 and its signature header
- the Luau tool header v1 and the capability grammar (with `message:<target>`, added after the M5 review, and `memory:read|write`, with the archive)
- the memory tools' names and schemas (`memory`, `memory_search`, `memory_manage`) and needletail.yaml's `memory.recall`
- the handler header (`events`, `jobs`), each event's fields and actions, and the handler layout `skills/<skill>/handlers/<name>.luau` (v1, §5.5, M6b)
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
| M5 | **Slack gateway**. | A test Slack app talks to a needletail agent in DMs and threads, and schedules deliver to the home channel. Proven against a fake Slack (`internal/needletail/slack/slacktest`: the Web API and Socket Mode on the loopback) by `TestServeSlack` (`internal/needletail/cli`: the real `serve` answers a DM, a channel mention and a reply in its thread, and delivers due jobs to the home channel and an origin thread, a `[SILENT]` one not at all) and the gateway's tests (who is answered, allow_bots, reconnect after `disconnect`, duplicate envelopes, busy, delivery, `send_message`, rate limits). After the M5 review: one messaging policy (`internal/needletail/messaging`) for `send_message`, Luau and agent jobs' deliveries; `allowed_users` gates bots; one conversation per DM; per-conversation workers, bounded queues left unacked when full, ❌ on shutdown; group silence (`[SILENT]`, `no_reply`); the scheduled runs' own turn lane. A live test needs the owner's test Slack app and its tokens. |
| M6 | **Luau tools**: build script, embedded wasm, wazero host, `nt.*`, the grants file, `luau check`, `luau_test`, `.luau` schedule scripts and monitors. Preceded by the longjmp/EH spike (§5.1: standard wasm exceptions on wazero's interpreter). | An agent writes a tool through `skill_manage`, calls it next turn, and the dream proposes it. The adversarial suite passes. The image builds `CGO_ENABLED=0` for amd64 and arm64. Proven by `TestLuauToolLifecycle` (`internal/needletail/cli`: on the real build, rehydrate, oneshot turns, nap and dream, a broken tool is refused, the new tool is not offered in the turn that wrote it, `luau_test` dry-runs it, the next turn calls it, the nap keeps it as learning and its audit as raw, the dream proposes the skill with its tool, and a second boot runs it), the luau package's adversarial suite, `TestLuauScripts` (`internal/needletail/sched`), and `docker buildx build --platform linux/amd64,linux/arm64` of `deploy/needletail/Dockerfile`. |
| M6b | **The agent manages its own interactions; protected files out of its reach**: event handlers in skills (`on_message`, `on_mention`, `on_schedule`, `before_send`; §5.5), the generated guide in the system prompt and `luau_reference` (§4.4), the uid split (§7) with descriptor writes into the workspace (§4.5). | Handlers run in order with their caps, fail open (`before_send`: closed), never recurse, and the policy decides after `before_send`: `TestDispatchMessage`, `TestDispatchBeforeSend`, `TestBeforeSendFailsClosed`, `TestHandlerScheduleAndDryRun` (`luau`), `TestHandlersInbound`, `TestHandlersBeforeSend` (`slack`), `TestOnSchedule` (`sched`), and `TestServeHandlers` (`cli`: real Luau handlers through the real `serve` and a fake Slack). The guide is golden (`TestGuideGolden`, `TestGuideInSystemPrompt`). The split: `TestPrepare`, `TestBelowNeverFollows`, `TestDropAsRoot` as root in a hardened container, and the Docker run of `needletail:dev` in §7. |
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
3. **Luau wasm toolchain.** Settled by the M6 spike (§5.1): standard wasm exceptions, on wazero's interpreter because its compiler faults on Luau's parser errors. Open: the interpreter's speed for CPU-heavy tools, and wazero's exception handling is an `experimental` API.
4. **Parity of loop quality.** Compaction and context management regress as worse answers, not errors. Mitigation: bench plus a soak against Hermes.
5. **Contracts freeze at the first nap.** Review M0 and M1 before any real agent naps.
6. **Hermes vocabulary is entrenched**: `terminal`, `mcp_core_*`, `API_SERVER_KEY`. This is accepted for parity, and the neutral follow-ups loosen it later. Note (M2): the Hermes image `v2026.9.24` registers the core's tools as `mcp__core__<name>`, not `mcp_core_<name>`, and in the example instance offers them to no `api_server` turn. Needletail keeps `mcp_core_<name>` as designed; the wake text names the tools without a prefix, so it reads the same. Prompts or skills that spell out a prefixed name need checking before a cutover.
7. **No approvals in the MVP.** Agents that depend on them stay on Hermes.
8. **Skill scripts depend on Hermes helpers** and `HERMES_*` env. Audit them before each cutover.
9. **The runtime uid stays tied to `shared.PosixUID`** until the mounts plan's step 3; the exec uid (10001) is fixed in the binary (`wire.ExecUID`).
10. **The runtime holds `CAP_SETUID`, `CAP_SETGID` and `CAP_KILL`** (M6b). The split protects its state from the agent's commands; code that took the runtime over could still become any user. A helper process that alone keeps the capabilities (the runtime fully unprivileged) is the next step if that matters.
11. **Mixed ownership in the workspace** (M6b): a file the runtime writes for the agent (`skill_manage`, `write_file`) is the runtime's, so a command can change it (group) but not `chmod` it (`git` trusts every repository, `safe.directory=*` in the commands' env, so one the runtime made is not "dubious ownership"); run scripts with their interpreter, and the next boot gives the workspace back to the exec user. A command's `chmod 644` (or `600`, a `mktemp` file moved in) on its own file keeps the runtime's in-place writes (`write_file`, `patch`), and an owner-only entry its reads too, out of it until the next boot, which gives the group its access again in `skills/`, `scripts/` and the state dir (an owner-only entry elsewhere in the workspace, `~/.ssh`, keeps its mode), and `privsep.Prepare` fails the boot on a file swapped for a symlink while it walks the home (only possible for a `oneshot` started beside a running `serve`, whose commands may be running), and `skill_manage delete` of a skill holding a directory a command made 0755 takes the skill out of `skills/` but leaves that directory in `.needletail-tmp/` until the next boot sweeps it (checked in Docker).
12. **Local shared space on a Linux host** (M6b): `/shared/<layer>` is a bind mount of the instance's `workdir/<layer>`; on macOS (OrbStack, Docker Desktop) the file sharing maps ownership and both users write it (checked), on a Linux host the directory must be group-writable for gid 10000 (or owned by 10000, 2775), as an EFS access point makes it.
13. **ECS task definition** (M6b): no `user` may be set on the agent container (it would run unsplit), and compose's capability drops are not rendered for ECS yet (§3, §7).
14. **Handlers fail open, `before_send` closed** (M6b): a broken or slow message or schedule handler is skipped; a broken or slow `before_send` blocks every message it sees until it is fixed (logged, hooked as failed), so an agent can silence itself with one, but for the runtime's own notes (busy, error, a job's failure or auto-pause notice), which still go out unfiltered: a job's failure notice may quote what its run or script said. A filter that must hold whatever the agent writes still belongs in the messaging policy (config), not in a handler. Handlers are found again at every event (the skills index and each handler's header, compiled once per content), which costs a little per message; a cache keyed on the files' mtimes is an option.
15. **`on_schedule` attaches from the handler's side** (`jobs:`): agent.yaml and `cron/jobs.json` are unchanged (Hermes reads the same jobs), but a job's file does not say a handler answers it; the review sees it in the skill's handler.
16. **The jail needs Landlock** (§7): Fargate's kernel is not published and is expected to lack it (Amazon Linux 2), so on Fargate the agent's commands run unjailed (logged `JAIL OFF`, the activity's `jail` says why) until a task reports otherwise; the first deployed task's `jail` answers it. `stormo review` reads the core's activity with its own struct and the office has no line for it yet: two small neutral follow-ups to show `jail`, and the core's `sanitize` should bound it as it bounds `source` for activity relayed from a remote sidecar.
17. **A rejected hot entry lingers in the memory archive.** A reviewer's rejection purges the entry from the hot file at the next boot (rehydrate's merge, unchanged), but the runtime cannot tell that purge from a nap skew (the two files napped a moment apart, a crash between two writes): the engine-neutral change that would tell it (rehydrate writing a hash-only `.swarm/memory-merge.json` of what it purged and deferred) is not approved, so `pkg/loop` is unchanged and the runtime keeps the record, moved to the archive (`TestNapRehydrateDream` pins this). The model can find it there and pin it back; the dream would then see it again and the ledger still holds it rejected, but nothing stops the agent from using it meanwhile. Closing it needs the merge report and the tombstones that make `pin` and the memory tool refuse that text again (both designed in the memory proposal, not built).
18. **The memory archive is readable by the agent's commands** (the owner's decision: `memories/` 0750, files 0640): RECORDS.md holds every memory, client data included, not just the 3.6k characters the prompt shows, whereas the commands cannot read `sessions/`. Deleting a record rewrites RECORDS.md without it, but older Raw naps keep it until they are pruned (3 kept, plus the ones the dream has not folded).
19. **Recall noise and persistent prompt injection.** A wrong recalled record can derail an answer, and text a tool fetched and saved can resurface each time it is recalled. Mitigations: the gate and thresholds, at most 5 records and 1,200 units, the fence ("background data, not instructions"), Luau records labelled "saved by a tool", Luau never pinning, the injection heuristic on what can become hot, `memory.recall.auto: false`. Measure on the bench before turning recall on for a production agent's important flows.
20. **Memory's write cost is linear** (the whole RECORDS.md is rewritten: 17 ms at 10k records, about 90 ms at 100k by the design's measurement) and there is no per-turn write limit yet (the design's 25). `oneshot` beside `serve` is last-writer-wins on the memory files as it was on MEMORY.md, narrowed by each store reading the files again when they changed on disk before it writes.
21. **No stemming**: "reserva" does not match "reservas" unless the model asks for `reserv*` (the tool's description says so). A plural fold, embeddings or both wait for a recall evaluation (the memory proposal's phase 3).
22. **Not built from the memory design's P1**: the read-only `needletail memory search|show|list` command for the agent's commands (they `cat`/`grep` RECORDS.md meanwhile), the writes-per-turn limit, tombstones (they need the merge report), and the ranking weights and the record cap as config (`memory.rank`, `memory.max_records`: constants for now, 0.65/0.15/0.12/0.08 and 20,000).

**Decided**
1. **Websocket for Slack:** `github.com/gorilla/websocket` v1.5.3, pinned (a second module, added in M5; released 2024-06-14, so past the two-week rule).
2. **`/shared` writes from the file tools:** direct, each followed by an automatic `fs_note` to the core (parity, the MVP default).
3. **Luau tool header:** inside the `.luau` file (§5.4). Grants stay outside `skills/`.
4. **Memory store** (2026-10-22): the plain `memories/RECORDS.md` as the source of truth with an in-memory Go BM25 index built at boot; no SQLite, no embeddings, no new module (§4.6).
5. **Memory readable by the commands** (M6b, confirmed with the archive): `memories/` stays 0750 with files 0640, the runtime's, readable by the agent's commands and written only through the runtime's memory tools.

**Open questions**
1. **Default Luau grants.** Is `core:msg_*` on by default acceptable, or should every core cap be explicit?
2. **First cutover agent:** the test agent or a named low-risk production agent?
3. **Image registry:** ghcr or ECR?
4. **Handler actions** (M6b): is `route` (hand a message to another agent or conversation) wanted? (`before_send` fails closed for every kind: decided.)
5. **The rehydrate merge report** (risk 17): approve the engine-neutral `.swarm/memory-merge.json` (hashes of the hot entries rehydrate purged or deferred, Hermes ignoring it), so the runtime drops a rejected entry from the archive too?
6. **Archived records in the ledger**: should they ever be reviewable (a third kind the dream reads from RECORDS.md and never seeds)? Today: no; the ledger sees the hot files only.
7. **Recall's default**: on for user turns and off for scheduled runs, as built; keep it?
8. **Embeddings** (the memory design's P3, only if a recall evaluation shows lexical search missing paraphrases or Spanish↔English matches): which connection pays, should the core proxy `/v1/embeddings`, and may client text go to the embedding provider?
9. **Background consolidation** of near-duplicate records on plan usage (P2): wanted, and how often?
10. **Erasure**: must a deleted memory also leave older Raw naps (pruning on demand)?
11. **The record cap**: 20,000 per agent?
