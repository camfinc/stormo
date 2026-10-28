# Needletail: where things stand (handoff)

Snapshot: 2026-10-28 (all five steps done; head `18a8c56`, pushed; origin == local). Start here after a context clear. Untracked status file (plan docs are not
committed until the owner says so); update it when the state changes.

## 1. Where the work lives

| What | Where |
|---|---|
| Engine branch | `camfinc/stormo` branch `needletail`, draft PR **#2** (https://github.com/camfinc/stormo/pull/2) |
| Worktree | `/Users/samt2497/camf/camf-swarm/stormo/.claude/worktrees/needletail` (a git worktree of the `stormo` submodule) |
| Branch head | `18a8c56`, **pushed** (Telegram/Slack before_send taint re-check fixes `64f4935`, `18a8c56` after step 5); step 5: `e94dd4e`..`268625b` on `ec36036`); before: `ef61a22` (step 4: `8888f9a`..`ef61a22` on `d19f05b`); before: `d19f05b` (step 3, files/images/voice: `a966871`..`d19f05b` on `38a2664`); before: `38a2664` (step-2 security fixes `6a66ceb`..`38a2664` on `90bfe82`); before: `90bfe82` (step 2: `28c09a8`..`90bfe82` on top of `75e0738`); earlier note: `13ff060` locally; **origin is at `9f000b1`** (7 local commits unpushed: `9daad6d`, `e0680a3`, `1937b35`, `54a8075`, `a1364b0`, `7836a3d`, `13ff060`) |
| Uncommitted | Nothing besides the untracked plan docs. Codex committed its in-progress work as `13ff060` ("reviewed background skill improvement and maintenance controls", 48 files, +3580; touches engine-neutral `pkg/loop/rehydrate.go`, new `pkg/learning/skill_review.go`). **`13ff060` is not yet validated**; it changed none of the files with §4 findings, so they all remain open. |
| Design | `docs/needletail.md` (the contract; §8 persisted formats, §9 milestones, §11 risks and open questions) |
| Other specs | `docs/needletail-quarantine.md` (quarantine, implemented by Codex in `9daad6d`), `docs/needletail-memory.md` (memory design, untracked), Codex's `needletail-autonomous-learning.md`, `-delivery-recovery.md`, `-learning-improvements.md`, `-recall-evaluation.md`, `docs/schedules.md` |
| Instance | `/Users/samt2497/camf/camf-swarm` (CAMF). `agents/mateo-nt/` is **uncommitted** (needletail copy of Mateo) |

Two parties have committed on this branch: Claude Code sessions (M0–M6b, follow_up, kv, listen,
session_search, background processes, trust mark, examples switch, app changes) and Codex (as
"Simon Mora": recall, learning outcomes/reflection, review UI, quarantine, delivery outbox, Slack
channel discovery, background learning).

## 2. What is built (on the branch)

Engine adapter and runtime (M0–M6b, see §9 of the design), plus:
- **Messaging**: one policy for `send_message`, Luau, job deliveries; broadcasts only with an
  explicit option and a config grant; `allowed_users` gates bots; group silence (`[SILENT]`,
  `no_reply`); one conversation per DM.
- **Luau**: wasm sandbox (Luau 0.740 on wazero's interpreter), 16+ globals from one registry,
  handlers (`on_message`, `on_mention`, `on_schedule`, `before_send`), key-value store, Slack
  channel listening mode (ambient messages go to handlers only; explicit `pass` starts a turn).
- **Memory**: `memories/RECORDS.md` (private 0600) + BM25 search, recall, audience scopes
  (`dm:`/`channel:`/`agent`), tombstones, `memory_search`/`memory_manage`, Luau globals; the
  engine-neutral rehydrate report `.swarm/memory-merge.json` v2.
- **Scheduler**: cron/interval, monitors, own job lane, limits and prompt scan; `follow_up`
  (one-time delayed messages and prompts, survives restarts).
- **Tools added for parity**: `session_search` (audience-scoped), background `terminal` +
  `process_manage`, `curl`/`jq` in the image.
- **Security**: uid split (runtime 10000, commands 10001), Landlock jail to the home and the
  agent's shared layers, read-only root, private archive; trust mark → **quarantine** (Codex,
  `9daad6d`): a conversation with unverified input can read and answer but not persist or forward.
- **Codex additions**: learning outcomes → review, bounded reflection, background learning and
  skill maintenance (opt-in), failed-delivery outbox with home alerts, Slack channel discovery.
- **Examples** (`examples/minimal`, office demo) run needletail; Hermes tests use
  `pkg/engine/hermes/hermestest`.
- **macOS app**: bundled CLI first, terminal link handling, follow-up refresh and labels,
  learning review UI.

Every Claude-built milestone and fix round was reviewed by Claude and Codex, findings fixed with
regression tests. Codex's own commits were validated on 2026-10-27 (§4).

## 3. Live state on the owner's Mac

- **CAMF Hermes agents** (clara, iris, leo, rigoberto, sofia): running on `nousresearch/hermes-agent:v2026.9.24`, healthy.
- **Mateo (Hermes)**: **stopped** (owner's go). Final nap kept in `agents/mateo/data/store`.
- **mateo-nt**: running `needletail:dev-18a8c56` (steps 1–5 live; files with vision and Spanish voice notes on; Luau ports of Mateo's scripts incl. the news radar active; approvals go to Simon), connected to Slack as Mateo's
  bot (it uses Mateo's Slack app; only one of them may run). Secrets in
  `agents/mateo-nt/data/secrets.yaml` (own API and core keys, Mateo's Slack tokens). Skills
  rewritten for needletail; `addressability` ported as a Luau handler.
- **Core**: still runs on **stormo main's** binary, which doesn't know needletail, so the core can't
  see mateo-nt's hooks/activity. Fix: restart the core on the needletail binary (owner action; the
  permission classifier blocked Claude from doing it): Core view › Restart in the app, or
  `stormo core down && <worktree>/bin/stormo core up`.
- **App**: Debug build from the worktree in the Claude job tmp folder (gone when the job is
  deleted); rebuild with `STORMO_BINARY=<clean build> xcodebuild -project apps/macos/Stormo.xcodeproj -scheme Stormo build`.
- **camf-swarm repo**: 6 unpushed commits on main (not from the needletail work); uncommitted
  edits to `agents/mateo/agent.yaml`, `agents/mateo/engine/hermes/config.yaml`, clara/leo/sofia
  skills (not from Claude's needletail sessions); `go test ./tests/` fails only on
  `unknown engine "needletail"` because of the uncommitted `agents/mateo-nt`.

## 4. Validation of Codex's commits (2026-10-27, at `7836a3d`)

Gate: `gofmt`, `go vet`, `go test ./...` pass; race run had one flake
(`runtimelearn TestBackgroundCandidateScopeAndUnknownVersion`, passes 5/5 alone); app builds, 91
Swift tests pass.

**Blockers before the branch can merge to stormo main** (engine-neutral, affects live Hermes agents):
1. HIGH: auto-review no longer approves Hermes lessons/skills: confirmations now come from
   `evidence-v1.json`, which only needletail writes (`pkg/loop/autoreview.go:77,96`,
   `pkg/loop/review.go:199`, `pkg/core/learn.go:410`). Gate the rule on `engine.OutcomeEvidence`;
   add a Hermes regression test (the existing ones were given fake evidence).
2. MEDIUM: ranking without evidence falls to ID order (`pkg/learning/ledger.go:218-232`); restore
   SeenCount/LastSeen tiebreaks.
3. MEDIUM: one corrupt evidence file breaks the learning compile for every agent
   (`pkg/learning/ledger.go:76`); warn and continue.
4. LOW: engine-neutral tests name needletail; ARCHITECTURE.md and stormo/CLAUDE.md not updated for
   the new interfaces and `evidence-v1.json`.
5. Product decision: nothing produces automatic evidence, so "auto" review needs the owner to
   confirm first. Decide and document (docs/core.md §6).

**Fixed 2026-10-27 (Claude, on `needletail`, unpushed; checks green incl. race, linux vet, Swift,
app build; camf-swarm `go test ./tests/` passes against it in a copy without `agents/mateo-nt`)**:
- 1 + 5: review rule per the owner ("AI decides what is obvious"): lessons reach the model by
  `auto_accept_min_seen`, skills by the old rules; evidence is the model's *input* (after the
  change's delimiters, a WARNING line on failures/corrections), never a gate. Hermes candidates,
  hashes, prompt and rules hash are byte-identical to 2980296 (cached verdicts stay valid);
  `auto_accept_min_confirmations` parsed but deprecated/unused (`3aaa216`).
- 2: rank falls back to seen count, last seen, id (`ebc4683`).
- 3: unreadable `evidence-v1.json` reads as none (logged, review `notices`); merge still refuses (`d1d36ff`).
- 7: dream never fails on a learning export: bad export/proposal/event or refused merge is a
  `DREAM.md` notice; flagged proposals kept flagged (`6b6bf05`); the runtime's export check uses
  the instance's scrub rules via a new `needletail.yaml` `scrub` key: roll image with compile (`d208969`).
- 4: engine-neutral tests use fake engines (`f9e25ab`, `3aaa216`); docs (`75e0738`).
- Race flake: not timing; 13ff060 made background version 2 valid while the old test used 2 as
  "unknown" (mid-edit run). Sentinels now 1<<30 (`6f17e6a`).
- 13ff060 bug: skill outcome events used the bundle revision, which the review never matches
  (skill evidence always empty); back to the snapshot hash, drafts bind the bundle revision (`fb7e9b0`).

**Before rolling quarantine and the outbox further on mateo-nt** (items 6, 8, 9: fixed 2026-10-28 by
step 2 of the plan, see below).

**Step 2 built 2026-10-28 (Claude, on `needletail`, unpushed; checks green incl. race, linux vet, the
Linux jail/privsep tests as root in a throwaway container `nt-s2-jail`, Landlock ABI 10)**:
- Part 0 (Codex findings): ledger upserts and reflection dedup union PII flags (`28c09a8`, Hermes
  path tested); a refused evidence merge keeps `watermark.json` `evidenceNap` behind and is retried,
  Prune keeps those naps (`4e1baec`).
- Slack approvals (`e5f7b25`): cards in the approvers' DMs (Block Kit, runtime-rendered: exact
  payload/diff, taint authors with permalinks, the agent's reason as its claim, sha256), Socket Mode
  `interactive` clicks, approver within allowed_users and never a taint author, hash-checked,
  single-use, runtime executes, card updated, outcome told and kept as a row; deny/timeout/boot-expiry
  deny; caps 3/conversation, 20/day; `approvals-v1.json` (Raw), `approvals-audit.jsonl` (State),
  hooks approval_waiting/answered; risky commands (`approvals.commands` over defaults) need approval
  anywhere, refused in scheduled runs; tainted scheduled reports held as cards (job keeps schedule).
- Information flow replaces quarantine (`d31b155`): tier table (allow / allow marked in
  `work/<conv>/` / approval / refuse), taint from data (marked replies, summaries keep marks,
  endorsement via "Approve & trust thread" or `POST /v1/sessions/{id}/trust`, audited), marked
  memory records (never hot/recalled; search marks the reader; owner pin endorses), context
  minimization + `thread_context`, `slack.relay_bots`, discovery refused in tainted/ambient runs and
  DM/mpim names never returned, tainted commands without core creds or http_secrets.
- Outbox (`f7d187a`): one home notice with count and job ids, updated in place, "recovered";
  alert only when retries spent or permanent; error_class; policy/unsupported permanent; shutdown is
  no failure, recovery skipped while stopping, TryLock; retarget limited to audience/home/approver DM;
  local jobs never hold; trusted Luau text follow-ups to top level again; `.tmp` swept at boot;
  classify.json cases. `9f0002f` (approved cron/skill/file/follow-up/pin actually run; taint why kept;
  guide), `71ece75` (docs).
- **Owner actions**: turn the Slack app's **Interactivity** on (Socket Mode: no URL); optionally set
  `approvals.approvers` (default: first `allowed_users` entry) and `slack.relay_bots` in
  `agents/<id>/engine/needletail/config.yaml`; roll image + compile together (new needletail.yaml keys).
- **Open** (as of step 2; see the security fixes below for what changed): approved follow-ups still
  deliver marked.

**Step-2 security fixes 2026-10-28 (Claude, on `needletail`, unpushed; Codex + Claude reviews, 21
items; each with a regression test seen failing first; checks green: gofmt, vet, test, race, linux
vet; Linux jail/privsep tests as root in `needletail:dev` throwaway containers `nt-s2fix-*`,
`--network none`, Landlock ABI 10)**:
- `6a66ceb` verified = listed in allowed_users (user or bot id) or, under allow_all_users, a full
  member of the agent's workspace (users.info: team, not guest/stranger/bot; cached 1 h); guests,
  Slack Connect/external users and unlisted bots are answered but taint. Without allow_all only the
  listed are verified (interpretation: the owner's rule "allowed_users is the direct gate").
- `319390f` cards show exactly what runs: canonical args (dup/case-variant/unknown keys refused),
  approval writes refuse symlink paths (and at execution), rich_text payload, model text as inline
  code/plain text, clipped within Slack limits (items 1, 2, 9).
- `6ba3921` before_send runs before the approval and the send is bound (`action.filtered`); memory /
  hot-entry / kept-report approvals show and bind the state (`action.bind`); "trust thread" only up
  to the request (`trust_at`, serialized with turns, refused if marked data came in since); deadline
  under the lock; `interrupted` at boot; outcome rows quoted as data and marked; clicks before
  wiring / during shutdown answered "try again" (items 3, 4, 5, 6, 7, 10, 11).
- `c349b29` approval lifts the gate, never the jail; `work/` 10000:10002 0710 (no trusted command
  reaches any work dir; tainted ones only their own, Luau too); tainted jail = home entries one by
  one without dotfiles/work, no TCP (ABI 4 required, else refused), scopes from ABI 6 (items 8, 12, 14).
- `3cf951f` marked memory text marks its reader (Luau search, similar, update echoes; model's
  memory_manage); tainted Luau http GET/HEAD only, no http_secrets (items 13, 15).
- `28cbff6` separate daily budgets (per taint author / tainted conv, trusted risky, held reports);
  home notice retries bounded per tried set (items 16, 17).
- `1cb5523` (engine-neutral, Hermes path tested) reviewer PII cleanup no longer re-flagged; stuck
  evidence watermark given up after 20 dreams / 7 days with a loud notice (items 18, 19).
- `b650ecf` thread_context honours its deadline; tainted pause of a trusted job and kill of a
  pre-taint process need approval (items 20, 21). `dbc875b` docs; `38a2664` jail test skip at ABI < 4.
- **Still open**: UDP/DNS from a tainted command (not a Landlock right); tainted Luau GET can carry
  data in a URL; tainted terminal refused on kernels < 6.7 (ABI < 4, e.g. AL2023 6.1); no end-to-end
  tainted terminal through `serve` with a real jail (launcher, privsep and DAC tested as root);
  an endorsed conversation's own `work/` is reachable by its file tools only, not its commands;
  held reports a before_send handler would change are not delivered (refused, not re-bound); a
  send approved before this change (unbound) whose text holds a redactable secret is refused as
  "changed" even with no handler (nothing posts; misleading message).
- **mateo-nt** (allow_all_users true, allowed_users [Simon], allow_bots mentions, listen ["*"]):
  workspace full members stay verified; guests, Slack Connect/external users and other agents' bots
  that @mention him (handoffs) now taint their conversations. List those bots' ids in allowed_users
  to keep handoffs verified. Roll image + compile together; `privsep.Prepare` re-groups `work/` at boot.

**Step 3 built 2026-10-28 (Claude, on `needletail`, unpushed; checks green: gofmt empty, `go vet ./...`,
`GOOS=linux go vet ./internal/needletail/...`, `go test ./...`, `go test -race ./internal/needletail/...`; the Linux
privsep/jail tests as root in a throwaway container were NOT run this step: `files/` 0700 under a real uid split
and the jail's skip of `files/` are unit-tested only)**: Slack files and images reach the model, voice notes are
transcribed.
- `a966871` llm/loop: images as OpenAI `image_url` content parts (data URLs); `files.vision` auto/on/off (auto:
  an endpoint refusing images, a 4xx naming them, gets the request again without them, for the process);
  `loop.Options.Images` with the turn's input in each request of that turn only, never in the session file;
  `tools.Env.Show` puts an image after its call's result (view_file); 4 per turn.
- `b187ba6` `internal/needletail/attach`: `files/<conversation>/<id>-<safe name>` + `.files-v1.json` index (author,
  `marked`, transcript), 0700/0600, by descriptor; Sweep (retention, orphans, 2 GB bound); stdlib image prep
  (sniffed, 50 MP bound before decode, box-downscale to 1568 px, JPEG/PNG; WebP as is ≤ 4 MB); `view_file`
  (list / show again / text fenced / transcript; marked file marks the reader). `files/` refused by the file tools
  and Luau, skipped by the tainted jail, `privsep.Private`, napped Raw (classify.json cases).
- `b624667` `internal/needletail/stt` (ElevenLabs `POST /v1/speech-to-text`, `xi-api-key`, `model_id` default
  `scribe_v2`, fixed host, no redirects, loopback-only `NEEDLETAIL_STT_API_URL` for tests, errors without key or
  body); config `files {retention_days, vision}` and `stt {enabled, provider, language, model}` in
  engine/needletail/config.yaml → needletail.yaml (validated; stt needs `ELEVENLABS_API_KEY` declared, which the
  runtime then owns: redacted and out of the terminal's env).
- `6314ae0` Slack: `files` on message events fetched in the worker after the handlers (bot token only to
  `https://*.slack.com`, redirects only there; ≤ 5 per message, ≤ 20 MB; images/text/PDF/audio incl. `slack_audio`
  clips; others named "not fetched"); file-only messages answered; input block per message (images shown, text
  fenced as the sender's data ≤ 50 KB / 100 KB a turn, PDF path + note, `[voice note transcript] …`); marked when
  the sender is unverified/relay; boot + hourly retention sweep; guide line (trimmed, 6976/7000 bytes).
  `d19f05b` 👀 before the download. `6c2fc2a` docs §4.1, §4.3, §4.5, §4.9, §6, §8, §11 (item 32).
- **Core gateway finding**: no change needed. `pkg/core/llm/translate.go` `userParts` already maps chat
  `image_url` (string or `{url, detail}`) to the Responses API's `input_image` (pinned by `gateway_test.go` ~329);
  the gateway reads request bodies up to 64 MB.
- e2e `cli.TestServeSlackFiles` (real serve, fake Slack serving files on loopback, stub model, stub STT):
  image → image part; CSV content in the input with markers neutralised; oversized/unsupported/redirected-off-Slack
  files named, not fetched; voice note → stub STT (key, model, language, bytes checked) → transcript in the input;
  view_file shows the image again in a later turn (after its result; the first turn's image not re-sent); a guest's
  upload stored marked and the conversation tainted; an upload 20 days old swept at boot; no image bytes in the
  session file; bot/app tokens and the STT key never in the log. Mutation-checked (fails without fetchFiles).
- **Owner actions**: add the Slack app scope **`files:read`** (reinstall the app); for mateo-nt, in
  `agents/mateo-nt/engine/needletail/config.yaml`: `files: {retention_days: 14, vision: auto}` and
  `stt: {enabled: true, provider: elevenlabs, language: es}` (omit `language` to auto-detect);
  `ELEVENLABS_API_KEY` is already declared (its agent.yaml comment "unused here" becomes stale; skill scripts can no
  longer read it from the env once stt is on). Roll image + compile together (new needletail.yaml keys).
- **Open** (§11 item 32): commands cannot reach uploads, so PDFs/documents are path + note only (no extraction, no
  script processing; a copy-to-workspace tool is the follow-up); WebP not downscaled; reading a marked upload
  re-marks a conversation even after endorsement; voice-note length bounded by size and Slack's `duration_ms` only;
  2 GB of uploads can make Raw naps that large; files in a thread's earlier messages not fetched; no `send_file`.

**Step 4 built 2026-10-28 (Claude, on `needletail`, unpushed; each item tests first, seen failing; checks green: gofmt
empty, `go vet ./...`, `go test ./...`, `go test -race ./internal/needletail/...`, `GOOS=linux go vet ./internal/needletail/...`)**:
- `8888f9a` item 6: Luau default write grant `fs.write:home/work/**` (own conversation's work dir only; a schedule's
  `.luau` script writes nothing in the home by default); explicit grants unchanged; migration note §5.4.
- `c72dbb8` item 3 (engine-neutral): stormo.yaml `core.llm_concurrency` (1-64; unset 3; `SWARM_CORE_LLM_CONCURRENCY` env
  still overrides); pkg/instance, pkg/core, docs/instances.md, docs/core.md, examples/minimal. Hermes unaffected.
- `ca8dcae` item 3: `limits.chat_turns` (1-16; unset 2) → needletail.yaml → API chat lane; §4.4 notes the core gateway bound.
- `7e4bcf9` item 1: `web_extract` (no key; `internal/needletail/web`, SSRF checks shared as `internal/needletail/netguard`
  with Luau; 2 MB fetch, 40 KB text, stdlib scan; marks the run; hooks `{has_url}`); Hermes `web` toolset → web_search +
  web_extract.
- `50e05f2` item 2: `web_search` (Brave `GET api.search.brave.com/res/v1/web/search`, `X-Subscription-Token`; Tavily
  `POST api.tavily.com/search`, `Authorization: Bearer`; as their docs said 2026-10-28); config `web.search {provider,
  max_results}`; compile refuses without the declared key; runtime owns the key; loopback-only test override
  `NEEDLETAIL_WEB_SEARCH_API_URL`; marks the run.
- `3d9b565` item 5: per-job `model`/`provider` applied (core gateway first when its /v1/models lists the model, else the
  provider or the agent's connection); needletail.yaml `model.providers` rendered only when a schedule names a provider;
  unknown/unkeyed provider = run error, no turn.
- `1cabe5a` item 4: handler `route` action (`to` agent id | `slack:<target>`, `note?`, `text?` ack), `route:<target>`
  capability (granted in `luau.grants`), msg_send with provenance or Slack post under before_send + policy, no turn; an
  unverified/relay/marked message's route goes to an approval card; group/unit:* refused.
- `ec46a19` item 7: guide line (reusable tools/job scripts as Luau, luau_test, terminal for one-off); guide 6974/7000.
- `6acb6e4` Codex step-3 upload findings 1-4: one image decode at a time runtime-wide and only when shown; view_file
  listing marks the reader and neutralises names; index bounded at 8 MiB per write, unreadable/oversized index rebuilt
  (entries marked; another version's left); 2 GB store bound reserved at each Save.
- `ef61a22` follow-up: `model.providers` always lists the agent's own provider; a route waiting for approval posts no
  acknowledgement; `to = "home"` refused.
- Codex step-4 findings 1-5 (each test first, seen failing; full checks green before each commit):
  `d179a3f` (HIGH) route: the conversation's durable taint is in the first decision; a route to Slack runs
  before_send once (sharing the handler's run mark), decides again on what it left and puts that final text to the
  approvers instead of posting it. `a286dce` route envelope: runtime header block (Author verified, From: user id +
  quoted one-line name, Where, quoted note), message fenced (longer than any backtick run, cut inside the fence),
  `[End of forwarded message]`; `[Forwarded from` / `[End of forwarded` added to the neutralised markers (text, note,
  subject); the core's msg_send has no provenance field, so no core change. `3063865` web fetch errors are fixed
  strings (class kept, HTTP status kept, only the asked host, "after a redirect"); no media type or redirect
  URL/host/scheme. `12520ee` extractor tag matching by byte-preserving ASCII folding (Kelvin sign / dotted I panicked
  it); `FuzzExtract` seeds. `9a57ad7` SetTranscript reserves its index growth against the 2 GB store bound (old
  transcript counted off; oldest uploads evicted; refused when the note + index cannot fit at all).
- **Owner actions**: roll image + compile together (new needletail.yaml keys `limits.chat_turns`, `web`,
  `model.providers`, and the default grant change). To enable web search on mateo-nt: add `BRAVE_SEARCH_API_KEY` (or
  `TAVILY_API_KEY`) to `agents/mateo-nt/agent.yaml` secrets and its value to `data/secrets.yaml`, then
  `web: {search: {provider: brave, max_results: 5}}` in `agents/mateo-nt/engine/needletail/config.yaml`. Route: grant
  e.g. `route:<agent id>` in `luau.grants` and declare it in the handler's caps. Optional: `limits.chat_turns`,
  stormo.yaml `core.llm_concurrency` (core restart). mateo-nt's Luau uses only kv caps: item 6 does not affect it.
- **Consequences**: web_extract/web_search mark the run: in a Slack DM (one continuing conversation) one lookup puts
  later sends elsewhere/skill/job/memory changes behind cards until "Approve & trust thread"; a scheduled job that reads
  the web has its report held for approval (unless it delivers only `local`). Tainted runs may still use both (reads:
  a URL/query can carry data out, as Luau GET; §11 items 30, 33).
- **Open**: the Linux privsep/jail root tests not run this step; no live web/search/route test; a pre-existing flaky
  temp-dir cleanup in `sched TestOutboxQuarantineNeverForwardsOrRetries` (passes rerun) and one unexplained cli flake
  (passed on rerun, not reproduced in 3 runs); route's approval path relies on `mcp_core_msg_send` being registered
  (core tools listed) to run once approved (the card is still posted without it: Canonical accepts any keys when the
  tool's schema is unknown; the approved call then fails as an unknown tool and the outcome says so).

**Step 5 built 2026-10-28 (Claude, on `needletail`, unpushed; checks green before each commit: gofmt empty, `go vet ./...`,
`GOOS=linux go vet ./internal/needletail/...`, `go test ./...`, `go test -race ./internal/needletail/...`)**: Slack slash commands
and a Telegram channel (design: docs/needletail.md §4.9 "Slash commands", §4.14, §7, §8, §11 items 34-35).
- `e94dd4e` `sessions.Store.Restart` (/new: a fresh child session, unmarked, `end_reason: new`); `f8626ef` `api.Server.Cancel`
  (/stop); `375746a` `internal/needletail/commands` (platform-neutral /new, /status, /memory, /jobs, /stop, /trust, /help; Slack
  names prefix + key, `/agent-status` because Slack reserves `/status`; reserved/malformed/duplicate names refused).
- `e4f70a0` Slack: `slash_commands` envelopes acked at once (no answer in the ack), answered off the reader through the
  `response_url` (Slack hosts only, no token, no redirect; loopback under the test override), fallback `chat.postEphemeral`;
  callers gated as message authors (admitted + verified; /trust approvers only, never a taint author); config
  `slack.commands {prefix, names}` → needletail.yaml (checked at compile and load). `d77265c` a command in a DM the agent is
  not in has no conversation.
- `06ed935` (engine-neutral, pkg/slack, tested with a fake engine) an engine may render its Slack manifest in process
  (`SlackManifest`, asked before `SlackManifestArgv`); needletail renders Socket Mode, Interactivity, events, the scopes its
  gateway uses (incl. `commands`, `reactions:write`, `assistant:write`, `files:read`) and its commands' configured names.
  Hermes unchanged.
- `c183985` shared file intake (`attach.Intake`; Slack's fetchFiles uses it, same text), `[Telegram message from` marker,
  `slack.OutcomeNotice`; `4ac8b7c` `internal/needletail/telegram` (+ `telegramtest` fake Bot API) and `telegram:` targets in
  `messaging.Parse`; `030f64b` serve wiring (TELEGRAM_* env from the adapter, token runtime-owned, one router for deliveries,
  Luau, follow-ups, approvals' notifier; API refuses `telegram-` ids; memory audiences `dm:telegram:<id>` /
  `channel:telegram:<chat>`; job origins; guide names Telegram targets only when it runs, golden unchanged); `b8b8abf` tests;
  `4c7f23c` docs (needletail.md, instances.md, ARCHITECTURE.md); `268625b` Telegram's command menu leaves out /trust, §4.9
  send_message names Telegram targets.
- **Decisions**: Telegram replies MarkdownV2 with a plain resend on a parse refusal; verified = listed (allow_all admits
  unverified, marked); groups mention/reply/command only (no listening mode); handler `route` not served on Telegram;
  approvals stay on Slack (no Telegram approvers: `/trust` unavailable there; Telegram-only agents refuse what needs
  approval); commands on Telegram answered in the chat (not ephemeral); `/new` clears the mark without endorsement (the
  owner's way, as asked) and leaves `work/<conv>/` reachable by file tools as endorsement does; `stormo check` keeps one
  `slash_commands: true` agent per instance.
- e2e (real serve, loopback fakes, stub model/STT): `cli.TestServeSlackCommands` (ephemeral answers, /memory audience
  scoping mutation-checked, /trust non-approver vs approver with audit, /agent-status, /new restart unmarked, /stop with no
  error note); `cli.TestServeTelegram` (DM in/reply out, stranger ignored, group chatter ignored and mention answered as a
  reply (mutation-checked), photo → image part, voice → stub STT transcript, send_message to home allowed and to an outside
  chat refused, repo job delivered to the home chat, agent job refused by policy and kept with the home notice, token/key
  never logged, offsets confirmed); `cli.TestServeTelegramUnverified` (allow_all: answered, input `[unverified`, session
  marked with `telegram:6666`). Unit: slack `TestSlashCommands` (ack before a slow answer, fallback, off-host response_url),
  telegram gateway/format/policy/follow-up/Tell tests, pkg/slack renderer seam, adapter env/manifest tests.
- **Owner actions**: (1) Slack: `stormo slack manifest mateo-nt`, paste into the app (Features → App Manifest), reinstall:
  adds `commands` (only if mateo-nt is the instance's slash-command owner), `reactions:write`, `assistant:write`,
  `files:read`, and replaces Hermes' ~50 commands by needletail's 7 (until then needletail answers Hermes' others "not one of
  my commands"). `stormo check` fails if both `agents/mateo` and `agents/mateo-nt` set `slash_commands: true`: keep it on one.
  Optional `slack.commands: {prefix: …}` in `agents/mateo-nt/engine/needletail/config.yaml` if another app in the workspace
  uses the same names. (2) Telegram: BotFather token as `TELEGRAM_BOT_TOKEN` in the agent's secrets (declare it in agent.yaml
  `secrets`), `channels: - kind: telegram, allowed_users: [<numeric ids>], home_channel: <chat id>`; decide BotFather privacy
  mode for groups (on by default: only mentions/replies/commands reach the bot, which is all it answers). (3) Roll image +
  compile together (new needletail.yaml keys `slack.commands`, `channels: [telegram]`).
- **Open**: no live Slack/Telegram test (fakes only); Telegram inline-keyboard approvals not built; no `send_file`, reactions,
  streaming or listening mode on Telegram; `route` not served there; Telegram updates confirmed once queued (a crash before the
  answer loses one); the Linux privsep/jail root tests not re-run this step (no change there).
- **Codex step-5 findings (2 HIGH) fixed 2026-10-28**, `64f4935`: Telegram `Send` and `Deliver` decided their quarantine before
  before_send, so a handler that read marked data and rewrote the message posted it to another allowed chat. `Send` now
  re-checks `quarantineSend` after the handlers (the final text goes to approval, bound + `Filtered`); `Deliver` re-checks
  `deliveryQuarantine` and returns `Deferred` (held), as Slack does. Audit of the other Telegram post paths: `FollowUp`
  already re-checks after the handlers (marks the destination); turn replies, notices, `Tell` and handler replies post only
  back to their own conversation; command answers, `AlertDeliveries` and the restart note run no before_send. Tests
  (`telegram/quarantine_test.go`, fail at 268625b): verdict-marked and ctx-marked rewrites → approval with the rewritten text /
  held report (failure notice too), no post; marked rewrite back to its own chat and clean rewrites still posted; follow-up
  rewritten from marked data marks its destination. `18a8c56` (owner pre-approved): Slack's `Send` re-check after the
  handlers returned the approval unbound (card showed the pre-handler text; approving re-ran the handlers and failed as
  altered); it now binds the final text (target, message, `Filtered`) and approving posts exactly that once
  (`slack.TestSendMarkedByBeforeSendBindsFinalText`, fails at 64f4935).
- **Slack private channels and channel reading, 2026-10-28** (`81e3614`, docs `c66fb9c`). Live gap on mateo-nt: in
  Simon's DM "puedes ver los canales de slack?" called `send_message {action: channels}`, which defaulted to
  `public_channel` and listed none of the private channels the bot is in (the token has `groups:read`/`groups:history`);
  and nothing could read a channel's messages. Now: discovery with no `channel_type` sends
  `types=public_channel,private_channel` and keeps a private channel only where `is_member` is true (labelled
  `is_private`), never a DM/group DM; still refused tainted/overheard. New gateway tool `slack_read` (channel or
  channel+`thread_ts`; ≤50 messages, 7 days default, 30 max, 40 KB, oldest first): lines labelled as thread_context
  (unverified/bot/you, markers neutralised, files named not fetched); an unverified or relay line marks durably then the run
  before the text (fail closed); bot must be a member; DMs/group DMs refused; public from anywhere (tainted too); private
  only when everyone the answer reaches is a member: the channel itself, a verified member's DM or an approver's DM, or a
  thread whose channel's members ⊆ the private channel's (and the run's verified requesters); no Slack conversation (job,
  API) → refused. `conversations.members` cached 2 min, ≤5 pages. Hooks: 12-hex channel hash + bounds. Guide line added
  (6997/7000 bytes). Tests: `slack.TestChannelDiscoveryDefaultIncludesPrivateMemberChannels`,
  `TestSlackReadLabelsLinesAndMarksOnUnverified`, `TestSlackReadPrivateChannelAudience`, `TestSlackReadThreadAndBounds`
  (slacktest gained conversations.list/history/members). No new Slack scope. Open (owner): a public read reaches whoever
  is in the conversation (a guest's DM under allow_all, a Slack Connect thread); the thread rule is stricter than
  "the requester is a member" (the whole asking channel must be inside the private one).

## 5. Remaining queue (owner-approved, not built)

1. §4 items 6, 8, 9 and approvals: built 2026-10-28 (step 2, §4). 13ff060 design concerns (full
   private stores, lost paid result on a failed staging, reconcile wedging the review page) are open.
2. Step-2 review findings fixed 2026-10-28 (§4, "Step-2 security fixes"). Next: a re-review of
   those commits, then build `needletail:dev-<sha>`, bump `agents/mateo-nt` (and decide whether to
   list the CAMF agents' bot ids in its allowed_users), turn on Slack Interactivity, restart.
3. Items 5 and 8: built 2026-10-28 (step 3, §4). Next: run the Linux privsep/jail tests as root for `files/`,
   add `files:read` to the Slack app, enable `files`/`stt` on mateo-nt, roll image + compile, and try a live image,
   CSV and voice note in a DM. Not built: `send_file`, document text extraction.
4. Built 2026-10-28 (step 4, §4): web search (Brave/Tavily) and web_extract, per-job model overrides, configurable chat
   slots and core concurrency, narrower Luau write grant, handler route; Codex's step-4 findings 1-5 fixed (§4).
   Next: a re-review of those fixes. Step 5 (slash commands, Telegram): built 2026-10-28 (§4), Codex's two HIGH Telegram
   before_send findings fixed (`64f4935`); next: the rest of the review of it, the
   owner's Slack manifest reinstall, and a live Telegram try with a test bot.
   Slack private-channel discovery and `slack_read` built 2026-10-28 (§4); next: a review, then a live try on
   mateo-nt (list channels in a DM, read a private channel you are in and one you are not).
5. M7 cutover (needletail agent in production): needs a live Slack test (approvals included), the
   core on the needletail binary, the §4 blockers fixed, merge to stormo main (owner's go), pointer bump in
   camf-swarm, and committing `agents/mateo-nt`.

## 6. How to build and roll out (dev)

```sh
WT=/Users/samt2497/camf/camf-swarm/stormo/.claude/worktrees/needletail
cd $WT && SHA=$(git rev-parse --short=7 HEAD)
gofmt -l . ; go vet ./... && go test ./... && go test -race ./internal/needletail/... && GOOS=linux go vet ./internal/needletail/...
go build -o bin/stormo ./cmd/stormo
docker buildx build -f deploy/needletail/Dockerfile --build-arg VERSION=0.1.0-dev.$SHA --load -t needletail:dev-$SHA -t needletail:dev .
# camf-swarm: set agents/mateo-nt/agent.yaml engine.version 0.1.0-dev.$SHA and image_tag dev-$SHA, then
cd /Users/samt2497/camf/camf-swarm && $WT/bin/stormo check mateo-nt && $WT/bin/stormo restart mateo-nt
```
Rollback to Hermes Mateo: `stormo stop mateo-nt` then `stormo start mateo` (never both: they share
the Slack app).

## 7. Rules that bit us

- Never run two Slack connections on one app (Mateo and mateo-nt).
- Build the app's bundled CLI from a clean `git archive` of a commit when others have uncommitted
  work in the worktree (`STORMO_BINARY`), otherwise half-done work ships in the app.
- Docker (OrbStack) hung once for hours; check with a timeout before blaming code.
- Live instance safety: never send requests to ports 18600-18700 from tests, never touch `swarm-*`
  containers from agents, never read secrets; restarting the core or OrbStack needs the owner.
