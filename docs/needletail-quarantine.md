# Needletail quarantine mode: implementation spec

Status: implemented on 2026-10-26; rollout pending the review gate below. Written 2026-10-26 against branch `needletail` at `9f000b1`
(stormo worktree `stormo/.claude/worktrees/needletail`). Line references below are from that commit
or from the Codex review of `4770a03`; find code by name if lines moved. Read `docs/needletail.md`
(§4.5 tools, §4.6 memory, §4.7 schedules, §4.9 Slack, §4.12 follow-ups, §5 Luau, §6 storage, §7
security, §8 contracts, §11 risks 27, 29, 30) before starting.

## 1. Problem

Needletail marks a conversation **untrusted** when content the agent must not obey enters it, and
then refuses a fixed list of deferred actions (prompt follow-ups, job create/prompt changes,
`skill_manage`, file-tool writes under `skills/` and `scripts/`, learning export). A Codex review
of `4770a03` showed the list approach leaks: untrusted text can still be **persisted or forwarded**
through other channels and come back later in a context that is trusted. Each new channel is a new
hole. Quarantine replaces the deny-list with one rule:

> Content from unverified sources may be read and answered, never persisted or forwarded so that it
> can act later under trust.

## 2. What exists today (build on it, don't duplicate)

- `messaging.Input` (`internal/needletail/messaging/messaging.go` ~L300-330): authors, `Verified`,
  `Ambient`, `Context`, `ContextUnverified`; `Input.Untrusted()` = `!Verified || Ambient ||
  ContextUnverified`.
- `messaging.WithUntrusted(ctx, why)`, `messaging.UntrustedWhy(ctx)` (~L339-370),
  `messaging.Deferred(what, why)` (~L373): the refusal error the model sees.
- `tools.UntrustedWhy(ctx, env)` (`internal/needletail/tools/tools.go` ~L27): run context, else
  `env.Untrusted(session)`; `cli.untrustedRun(store)` wires the store's mark (`cli/cli.go` ~L208).
- Persisted mark: session header and message rows carry optional `untrusted` (sessions JSONL v1,
  additive); compaction copies it; `sessions.Store.WithTrusted(id, fn)` (`sessions/sessions.go`
  ~L350) runs `fn` only if no session in the lineage is marked (used by `loop/learning.go` for the
  learning export: keep that working).
- Current gates: `followup/manage.go`, `cli/followup.go`, `sched/tool.go`, `tools/skills.go`,
  `tools/files.go` (skills/ and scripts/), `loop/learning.go`.
- Jail: `internal/needletail/jail` (`Spec{Home, Shared}`, `Spec.Rules()`), launched for every
  command through `jail.Launcher.Wrap` from `tools.Command`; uid split in `internal/needletail/privsep`
  (`Workspace = []string{"skills","scripts"}` writable by the exec uid 10001).

## 3. Decisions (owner-approved direction)

### 3.1 What marks a conversation (narrowed)

Mark (set `untrusted`, persisted, never cleared automatically) when any of these enters the
conversation:
- a message from an author **not** in `allowed_users` (`!Verified`), including bots not listed;
- `[unverified]` thread-context lines (`ContextUnverified`);
- a delivered follow-up text that is `untrusted` or Luau-made (`by: luau:…`);
- the input of a Luau-made prompt follow-up's turn;
- **new:** any tool result or Luau return that carries marked data (see §3.3 propagation).

**Change:** an **overheard (ambient) message from a verified author no longer marks** by itself.
Rationale: with `listen` on, a verified teammate's chatter in a channel the agent listens to is
trusted company content; marking it quarantined nearly every listened thread. Ambient *unverified*
authors still mark (they're `!Verified`). Keep the `Ambient` flag on `Input` for handler logic and
for the overheard framing in prompts; just drop it from `Untrusted()`.

### 3.2 What quarantine allows and blocks

A run is **quarantined** when `tools.UntrustedWhy(ctx, env) != ""` (its context, or its
conversation's persisted mark). In a quarantined run:

| Channel | Today | Quarantine |
|---|---|---|
| Reply in its own conversation | allowed | allowed (policy + before_send as usual) |
| Read tools (`read_file`, `search_files`, `skill_view`, `skills_list`, `memory_search`, `session_search`) | allowed | allowed; results may mark the reader (§3.3) |
| `terminal` (foreground and `background:true`) | allowed, home writable | allowed with a **read-only home**: Landlock grants read on the home and the shared layers, read+write only on the private temp dir (`<home>/.tmp`, `TMPDIR`); no write to `skills/`, `scripts/`, memories, the state dir, or `/shared` |
| `write_file`, `patch` | refused under skills/, scripts/ | refused everywhere except under `<home>/.tmp` |
| `memory` (add/replace/remove), `memory_manage` (save/update/delete/pin/unpin) | allowed | **refused** (`messaging.Deferred`) |
| Luau `save_memory` / `update_memory` / `delete_memory` | allowed | refused |
| `send_message` | allowed by policy | allowed only back to the conversation's own origin (it is a reply); refused to any other target |
| `mcp_core_msg_send` (bus to other agents) and Luau `core_call` of any **write** core tool (`msg_send`, `fs_write`, `fs_note`, `fs_lock`, `cap_start`, …) | allowed | **refused**; read core tools (`msg_inbox`, `msg_read`, `fs_ls`, `fs_info`, `cap_status`, …) allowed and mark the reader if they return marked data (bus/shared provenance is out of scope here: see §6) |
| Luau `write_file` / `append_file` / `remove_file` | allowed by grant | refused except under `.tmp` (Luau has no `.tmp` path today: simply refuse) |
| Luau `kv_set` / `kv_delete` | allowed | allowed, but the stored value is **marked** (§3.3); `kv_delete` of a marked key allowed, of an unmarked key refused |
| `follow_up` (tool and Luau) | text to own conversation, untrusted | unchanged (text to own conversation only, stored untrusted) |
| `cronjob_manage` | no create / no prompt changes | unchanged; additionally refuse `run` (an on-demand run would execute a trusted job inside an untrusted turn's timing — decide; recommended: allow, the job runs in its own trusted context and sees nothing from this turn) |
| `skill_manage` | refused | unchanged |
| `luau_test` (dry run) | allowed | allowed (no writes by design) |
| `process_manage` | allowed | allowed for processes started in this conversation (they were started jailed read-only if quarantined) |

The model gets one `messaging.Deferred` error per refusal naming the reason, and the guide (one
line, `cli/guide.go`, MaxGuide 7000) says: in a conversation with untrusted input you can read and
answer, but nothing you do there is kept or forwarded.

### 3.3 Propagation on read (taint the reader)

Reading marked data quarantines the reader **before** the data reaches the model:
- `session_search` hits/scroll/read/browse that include a marked conversation's rows → mark the
  calling conversation (persist the mark first; if persisting fails, return an error instead of the
  data). Today they only add an `untrusted` field.
- `kv_get` of a marked value → the run becomes untrusted (context) and its conversation is marked
  when the Luau tool result is stored. Store the mark next to the value in `kv.json` (v1 additive
  `untrusted` per entry).
- Untrusted scheduled jobs (`"untrusted": true` on an agent-made job): the turn must persist the
  mark on its session. Fix: derive the persisted mark at the central turn boundary from
  `messaging.UntrustedWhy(ctx)` in addition to `Options.Untrusted` (`sched/sched.go` ~L559-563,
  ~L960; `loop/loop.go` ~L194).
- Delivered untrusted follow-up texts into Slack: create and durably mark the **destination**
  conversation before posting (including explicit targets), and treat a failed append/header rewrite
  as a failed delivery (`cli/followup.go` ~L132-158). Today the post can succeed with no mark.
- Bot posts the agent itself made in a marked conversation, read back later as thread context, must
  not launder the mark: thread-context assembly (`slack/gateway.go` ~L731) should carry the mark of
  the session that produced the bot's text, or treat the agent's own posts in a marked thread as
  marked context.

## 4. Codex findings this closes (review of 4770a03)

1. High: terminal bypasses deferred-execution restrictions (`tools/terminal.go` ~L66-117; scheduled
   scripts run what it writes, `sched/script.go` ~L27-55) → read-only-home jail when quarantined.
2. High: memory writes promote untrusted text into future system prompts and learnings
   (`tools/memory.go` ~L105-129, ~L242-351; `loop/loop.go` ~L472-484; Luau `luau/memory.go`
   ~L133-170; dream harvest `pkg/loop/dream.go` ~L245-256) → refuse memory mutations when
   quarantined.
3. High: `msg_send` launders content into another agent's trusted API run (`mcp/mcp.go` ~L289;
   Luau `luau/host.go` ~L732-768; bus wake `pkg/core/bus.go` ~L388-396; API wake runs with empty
   options `api/api.go` ~L519-538) → refuse bus/core writes when quarantined.
4. High: `session_search` doesn't propagate taint to the reader (`history/tool.go` ~L72-79) → §3.3.
5. High: Slack follow-up delivery can succeed without persisting its mark → §3.3.
6. High: shared files as a cross-conversation channel (`tools/files.go` ~L64-75, ~L337-348; Luau
   shared writes via `fs_write`, `luau/host.go` ~L591-596) → refuse shared writes when quarantined
   (terminal by the read-only jail, file tools and Luau by the gate).
7. Medium: Luau KV drops provenance between runs (`luau/kv.go` ~L235-305) → marked values (§3.3).
8. Medium: untrusted scheduled runs leave unmarked transcripts → §3.3.

## 5. Implementation notes

- **One gate function.** Add `messaging.Quarantined(ctx) string` (or reuse `tools.UntrustedWhy`) and
  call it at each write point listed in §3.2; return `messaging.Deferred(what, why)`.
- **Jail.** Extend `jail.Spec` with `ReadOnlyHome bool` and `Writable []string` (the temp dir). In
  `Spec.Rules()`, when `ReadOnlyHome`, give the home and shared layers read (+execute for the home's
  `scripts/`/skills scripts so read-only use still runs them) and the temp dir RW. `tools.Command`
  passes `ReadOnlyHome` when `UntrustedWhy` says quarantined. Background processes keep the spec
  they started with. Where Landlock is absent (`jail` reports off, e.g. Fargate), quarantined runs
  must **refuse `terminal` entirely** (fail closed) and say why.
- **Ordering.** Persist the mark before any marked data reaches the model; if persisting fails,
  fail the call (fail closed), never return the data.
- **Contracts (all additive, document in §8):** `kv.json` entries get optional `untrusted`; nothing
  else new beyond what §3.3 implies. Bump nothing; older images ignore unknown keys? (They don't:
  `KnownFields(true)` on needletail.yaml only; JSON stores tolerate extra keys — verify per file.)
- **Docs:** `docs/needletail.md` §4.5, §4.6, §4.9, §4.12, §5.3, §7 (replace the "Untrusted
  conversations" bullet with "Quarantine"), §8, §11 (rewrite risks 27, 29, 30); the guide line.

## 6. Out of scope (write them down as §11 risks)

- Provenance for data that leaves needletail before this ships (existing shared files, bus messages
  already sent): not tracked.
- Bus and shared-file provenance across agents (marking another agent's reader when it reads a
  marked file or message): needs core support; quarantine prevents new marked writes instead.
- Network exfiltration from a quarantined terminal (it can still `curl` out): quarantine is about
  persistence and forwarding into trusted contexts, not about data leaving. If wanted later, add a
  Landlock network rule or a per-run egress allowlist.
- Hermes: unchanged.

## 7. Acceptance tests (write each first and see it fail)

Use the in-process `serve` + `slacktest` fake Slack + stub model pattern from
`internal/needletail/cli/*_test.go`.
1. Verified teammate's ambient message under `listen` → turn runs, conversation **not** marked;
   an unverified author's message → marked.
2. Quarantined turn: `terminal` `echo x > ~/scripts/a.sh` fails (read-only), `echo x > $TMPDIR/a`
   works; same with `background:true`; on a no-Landlock jail, `terminal` is refused.
3. Quarantined turn: `memory add`, `memory_manage save`, Luau `save_memory` refused; hot files and
   RECORDS.md unchanged.
4. Quarantined turn: `mcp_core_msg_send` and Luau `core_call("msg_send")`, `fs_write`, refused;
   `msg_inbox`/`fs_ls` allowed.
5. Quarantined turn: `write_file` to `/shared/group/x.md` refused; to `<home>/.tmp/x` allowed.
6. `session_search` from a clean conversation that returns a marked conversation's rows → the clean
   conversation is now marked (persisted; survives restart) and a following `follow_up` prompt is
   refused.
7. KV: handler on unverified input `kv_set("k", text)`; later clean tool `kv_get("k")` → its run is
   untrusted and its conversation marked.
8. Untrusted agent job run → its session file header is marked.
9. Untrusted follow-up text to a Slack DM with no session yet → session created and marked before
   the post; a forced append failure → delivery fails, no post.
10. Learning export (`loop/learning.go`) still refused for marked conversations (regression guard
    for the other session's work).
11. Untainted conversations: every tool behaves exactly as before (table test over the §3.2 list).

## 8. Rules for the implementing agent

- Work in the worktree above, on branch `needletail`; another session is committing learning work
  on the same branch: pull nothing, but run `git log` first and don't rewrite others' commits.
- Tests first; small commits; messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Gate before each commit: `gofmt -l .` empty, `go vet ./...`, `go test ./...`,
  `go test -race ./internal/needletail/...`, `GOOS=linux go vet ./internal/needletail/...`.
- No new Go modules. Nothing outside `pkg/engine/needletail`, `internal/needletail`,
  `cmd/needletail`, `deploy/needletail`, docs and the registry names needletail. Hermes unchanged.
- Never touch ports 18600-18700, `swarm-*` containers, real Slack, or secrets. No push, no branch
  operations, no stash.
- After review (Claude + Codex), roll out: build `needletail:dev-<sha>`, bump `agents/mateo-nt`
  `engine.version`/`image_tag` in camf-swarm (uncommitted folder), rebuild the app's bundled CLI
  from a clean archive, restart `mateo-nt`.

## 9. Implementation decisions and verification

- Verified ambient input is trusted unless another provenance trigger applies. Existing persisted marks remain monotonic.
- `cronjob_manage run` stays allowed as recommended: the existing job receives no caller content and executes in its own context. A job or handler that reads marked KV retains that mark through gate caching and its model turn; marked scheduled results cannot be forwarded.
- Quarantined commands require Landlock ABI 3 or newer (ABI 1/2 cannot reliably deny truncation). They keep their launch policy in background mode. A newly quarantined conversation cannot write/submit/close stdin of a process launched while trusted; it can still inspect or kill its own process.
- Marked follow-up delivery to a top-level channel without a canonical thread is refused: Slack supplies that thread id only after the post. Known threads and DMs are created/marked durably before posting, including a DM with no previous local session.
- KV v1 uses additive `untrusted` metadata. Legacy JSON readers accept it, but a legacy writer drops it on rewrite: downgrade is not a safe provenance rollback. Preserve the marked store and disable old KV writers if reverting code.
- Direct built-in and Luau file reads of `sessions/`, `state/`, and `logs/` are refused even for trusted runs; use the scoped managing tools. Otherwise a clean reader could bypass history/KV propagation by reading raw files. Shell isolation still assumes the production runtime/exec uid split. The approved `.tmp` scratch exception is not a provenance store for later trusted work.
- Existing shared/bus content has no retroactive provenance; cross-agent provenance, terminal network egress controls, and Hermes remain out of scope.

Tests first reproduced the verified-ambient regression, lost scheduled context mark, memory/file/MCP bypasses, history propagation, KV provenance loss, and missing destination persistence. The implemented regressions cover trusted controls, same-run propagation and persistence failures. Independent GPT-6.1 Sol cross-review also identified and closed monitor-baseline, public error-summary, and direct raw-file read laundering paths. An isolated no-network Linux container on Landlock ABI 10 verified actual read-only home/shared behavior, denied truncation and background writes, permitted private temporary writes, and rejected symlink escapes. No real Slack, secrets, swarm containers, or ports 18600–18700 were used.

Rollout is not part of the completed validation: this document requires both Claude and Codex review before changing the instance, packaging its CLI, or restarting its agent. GPT-6.1 Sol implementation/review is not a claim that the independent Claude review occurred.

### Regression coverage

| Acceptance area | Coverage |
|---|---|
| Verified ambient versus unverified input | `messaging/quarantine_test.go`, `slack/trust_test.go` |
| Read-only foreground/background jail and no-Landlock refusal | `jail/landlock_linux_test.go`, `tools/quarantine_test.go` |
| Memory, persistent files, bus/core writes; trusted controls | `tools/quarantine_test.go`, `mcp/quarantine_test.go`, `luau/quarantine_test.go` |
| History reader quarantine and failed persistence | `history/tool_test.go`, `sessions/quarantine_test.go`, `loop/quarantine_test.go` |
| KV restart/same-run provenance, caught errors, handler propagation | `luau/quarantine_test.go` |
| Scheduled input marks, cached monitor gates, private diagnostics | `loop/quarantine_test.go`, `sched/quarantine_persistence_test.go` |
| New-DM follow-up marking, append failure, own-bot context, send scope | `cli/taint_test.go`, `slack/quarantine_test.go` |
| Learning-export refusal | Existing `loop/outcomes_test.go` and learning API regression suite |

Tests use synthetic local fixtures. The Linux jail check uses an isolated disposable container, not the live deployment.

Validation completed on 2026-10-26 for implementation commit `9daad6d`: `gofmt -l .` empty, `git diff --check` clean, `go vet ./...`, `go test ./...`, `go test -race ./internal/needletail/...`, and `GOOS=linux go vet ./internal/needletail/...` all passed. The final archive inode guard also passed a fresh tools race run. Runtime/kernel evidence is limited to the isolated Linux fixture above; no live deployment or real-provider/Slack verification is claimed.
