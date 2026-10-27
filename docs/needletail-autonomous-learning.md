# Needletail autonomous learning and skill maintenance

This is the implementation plan for learning that continues between foreground tasks,
turns verified experience into reviewable improvements, and keeps skills current.
The first implementation batch adds a durable background proposal queue and a worker,
plus ownership-aware, dry-run skill maintenance foundations. It does not complete autonomous
activation, maintenance scheduling, interface parity, or a measured production rollout.
No deployment is implied by a local build, passing tests, or a commit.

Read this alongside [the learning correctness plan](needletail-learning-improvements.md),
[the architecture](../ARCHITECTURE.md), and [instance contracts](instances.md).
The existing recall, evidence, quarantine, PII, review, and restoration protections
remain requirements rather than optional improvements.

## Current behavior and the boundary being changed

`runtimelearn.Store` records versioned outcomes in a private store. Runtime observations,
authorized feedback, and assertions remain distinguishable. A view, successful shell
exit, snapshot sighting, or model assertion alone does not establish task success.
The current `Reflect` path accepts an explicitly supplied bounded summary, finds related
memory, and asks for at most one proposal. It has no tools and does not activate memory,
skills, schedules, or policy. Export is an explicit selection of scrubbed agent-wide
procedural material into the existing dream/review path.

Reflection has historically run during feedback handling and reserved one job per task
with a lifetime limit of 100 calls. That reservation prevents accidental repeated cost,
but inline execution couples owner requests to model latency and does not describe
queued, cancelled, failed, or interrupted work well. Background execution must preserve
that conservative accounting while making pending work durable and observable.

Runtime `skill_manage` already permits trusted tasks to edit agent-owned skills. Its
existing foreground behavior is distinct from the new background curator: permission
to edit a skill during a trusted requested task does not give a background model blanket
permission to rewrite every skill. Generated skills are read-only. Repository acceptance
and runtime edits remain separate operations.

## First batch implementation inventory

The additive queue is `state/needletail/background-learning-v1.json` (0600, Raw),
separate from legacy reflection. `EnqueueBackground` admits evidence-backed triggers;
`ProcessBackground` handles one job; `RecoverBackground` marks startup reservations
interrupted; `BackgroundSnapshot` returns scoped candidates and job counts together
with home-wide rolling budget usage. The initial adapter uses only the selected bounded
summary and opaque evidence references. Related-memory lookup and duplicate semantic
search in this new adapter remain future work.

Current queue defaults are **8 calls and 4,096 reserved output tokens per rolling 24
hours**, 512 tokens per call, a 30-second deadline, and **256 cumulative job identities**.
A reservation is charged even on model failure. The maximum is two attempts only when
an adapter explicitly identifies a failure as known safe to retry; ordinary provider
errors are terminal. Stored states are `pending`, `reserved`, `complete`, `failed`, and
`interrupted`. No terminal-job pruning or owner cancellation exists in this batch.
Capacity exhaustion is deliberate and visible rather than silently losing deduplication.
The richer lifecycle below is the roadmap, not the initial persisted format.

`background_learning` is an engine runtime opt-in; only the serve process starts its
worker. It checks for active foreground turns before each 15-second tick. Corrected
authorized feedback and observed failed skill executions are first trigger integrations.
Trusted success/recovery admission exists at the queue interface, while automatic runtime
success attribution and delivery-recovery reflection remain deferred. Candidates are
new review material with host-assigned scope and evidence references. They cannot
activate skills, mutate hot memory, export themselves, or declare verified task success.

The owner learning API adds a separate `background` field so these proposals are not
confused with legacy exportable reflection candidates. Browser/native presentation,
cancel/retry controls, retention controls, maintenance scheduling, automatic model-generated
skill patches, activation, and production quality evaluation remain subsequent work.
Final verification results must be reported by the implementation handoff; this document
is not a passing test receipt.

## Target architecture

```text
trusted runtime observation / owner feedback / assertion / due maintenance scan
    -> validate origin, root, audience, quarantine and subject revision
    -> bounded durable queue (private metadata and selected summary)
    -> single background worker with reserved call budget
    -> related-record lookup + proposal-only model request
    -> candidate validation + second authority/revision check
    -> private candidate store
    -> explicit eligible export / deterministic evaluation
    -> applicable human review or narrowly authorized activation policy
    -> compare-and-swap activation + receipt + rollback record
```

The runtime owns observations and the private proposal queue. The core owns fleet-wide
review orchestration and owner controls. Skill storage owns revision calculation and
atomic activation. Engine-neutral proposal, evidence, and review structures belong in
`pkg/learning`; runtime-specific execution stays in `internal/needletail/runtimelearn`
and its loop integration. Other engines receive equivalent behavior through engine
interfaces rather than imports of Needletail internals.

There is one runtime writer per home. A queue mutex alone is not an interprocess lock:
starting a second worker against one home must be refused or protected by a home lock.
Foreground turns take priority. Workers never block a conversation for a model call;
they use short critical sections for queue changes and final guarded persistence.

## Automatic triggers

| Trigger | Admission rule | Candidate purpose |
|---|---|---|
| Explicit owner correction | Authorized feedback tied to an observed exact revision | Correct a mistaken fact or procedure |
| Failure followed by recovery | Same subject revision; recovery has independent task-specific verification | Explain a reusable recovery with its limits |
| Repeated verified use | At least two distinct task roots and distinct underlying origins | Propose a reusable procedure |
| Runtime failure | Record evidence immediately; no fabricated lesson or success | Accumulate a maintenance signal |
| Skill due for review | Revision inventory plus configured due date or freshness class | Propose revalidation, documentation update, or deprecation |
| Conflicting evidence | Current revision has contradictory verified results | Suspend auto eligibility; request review |
| Restore/startup | Resume eligible queued work; inspect interrupted reservations | Recovery only, not a fresh learning trigger |

Routine turns, skill views, snapshots, successful transport delivery, and model-reported
completion do not independently schedule paid reflection. Scheduled report delivery
recovery proves transport recovery only; the original workload can remain failed.
The first batch wires authorized corrections and failed skill executions into the
opt-in background path. A failed execution can justify a diagnostic proposal, but cannot
establish successful recovery or verified task success. Other automatic sources require
explicit integration and their own acceptance tests before being advertised as active.

Trigger IDs are deterministic. Include task root, audience, subject identity and revision,
trigger kind, originating event IDs, and rule version. Multiple tool calls, retries,
compaction, replayed feedback, or repeated scans cannot manufacture independent jobs.
A materially new revision is eligible for fresh evaluation, within its budget.

## Durable queue contract

Use a separate versioned private runtime file under `state/needletail/`. Do not reinterpret
`reflections-v1.json` jobs as a complete queue. Queue state contains no raw conversation,
full provider error, delivery address, token, or unrestricted archive excerpt.

Recommended interfaces (the exact first-batch API may be smaller):

```go
Enqueue(ctx, Trigger, BoundedSummary) (JobReceipt, error)
Claim(ctx, now, WorkerID) (Job, bool, error)
Complete(ctx, JobID, ReservationID, Result) error
Cancel(ctx, JobID, ExpectedRevision) error
Status(ctx, AudienceScope) (QueueStatus, error)
```

A job records format version, ID, trigger/event references, root, audience, session
reference, subject revision, summary, creation/due timestamps, state, attempts,
reservation, bounded result code, candidate IDs, and rule/model/prompt versions.
Session references are private and revalidated at execution; a missing session never
silently becomes agent-wide authority. Store a revision or generation for cancellation
and concurrent owner changes. Use opaque error codes in owner summaries.

Planned lifecycle:

| State | Meaning | Recovery |
|---|---|---|
| queued | Durable eligible job, no paid call reserved | May run after restart |
| running | Worker owns durable reservation | Inspect on restart; do not blindly repeat |
| completed | Valid candidate or deliberate empty result | Terminal and idempotent |
| failed | Validation, model, storage, or authority failure | Expose reason; retry only under bounded explicit policy |
| cancelled | Owner disabled/cancelled or source invalidated | Terminal; retain deduplication identity |
| uncertain | Model may have been called before interruption | No automatic paid retry without a new authorized reservation |

Reservation is persisted before calling the model. Exactly-once execution cannot be
promised across process death and a remote response. At-most-once automatic calls are
preferable in the first batch: ambiguous reservations consume budget and require owner
review. If persistence fails after receiving output, preserve the reservation; do not
claim completion or automatically spend again. An empty result is successful completion.

A later lease scheme must separate expired local ownership from uncertainty about a
remote paid call. Only failures known to precede a call are safe for automatic retries.
Use bounded exponential backoff for those failures; never an unbounded hot retry loop.

## Budgets, defaults, and retention

Keep background proposal generation opt-in initially through `background_learning`.
Outcome evidence collection remains enabled in shadow mode where already supported.
Enabling proposal generation does not enable activation. Existing `reflection`
configuration retains its legacy 100-call lifetime ceiling independently of the new
background queue; the queue must own explicit durable budgets and must not reset legacy
reservations. The existing reflector uses 512 output tokens, a 30-second deadline, a
2,000-byte summary, an 8,000-byte input ceiling, and 100 candidates. New background
limits must be explicit and tested rather than silently inherited.

Proposed operational defaults for the subsequent configurable scheduler are:

| Control | Proposed default | Behavior at limit |
|---|---|---|
| Concurrent model jobs per home | 1 | Leave work queued |
| Paid calls per day | Current initial default: 8, independent of legacy reflection budget | Pause with explicit budget reason |
| Model output per call | 512 tokens | Reject oversized/invalid result |
| Summary/input | 2,000 / 8,000 bytes | Refuse rather than truncate meaning silently |
| Queue capacity | Current initial default: 256 cumulative jobs | Reject new admissions; record bounded saturation metric |
| Attempts | 1 paid attempt | Interrupted calls remain reserved/uncertain |
| Maintenance scan | Daily, bounded inventory, no paid call by default | Resume bounded cursor on later scan |
| Terminal job detail | 30 days | Compact only after preserving required dedupe/decision data |
| Candidate review | Due within 30 days unless explicitly pinned | Mark due; do not silently activate or delete |

These new operational values are proposed defaults, not a claim that all settings exist.
Any instance setting belongs in `pkg/instance`, `docs/instances.md`, and
`examples/minimal`; engine-specific runtime settings follow the existing manifest/compiler
contract. Do not insert an undocumented YAML key and assume it works.

Retention must preserve rejection tombstones, revision lineage, uncertain reservations,
rollback receipts, and evidence summaries needed to prevent replay or resurrection.
Never expire a dedupe identity while its source remains replayable. Cap both item count
and encoded bytes. A full store fails closed and reports saturation instead of erasing
unreviewed evidence. Retention is a separate versioned compaction operation, initially
owner-invoked; it must not reset paid-call counters or invent independent confirmations.
Private evidence remains private after compaction. Deletion follows its source retention
policy and legal owner direction rather than indefinite copying into aggregate text.

## Proposal, evaluation, activation, and rollback

Every proposal identifies action (`new`, `update`, `supersede`, `deprecate`, or
`revalidate` as appropriate), exact base revision, supporting events, checks, conditions,
exceptions, audience, sensitivity, review date, and intended artifact. Unsupported or
contradictory claims stay visible as uncertainty. The generator cannot approve itself.

Evaluation proceeds through deterministic validation, audience/secret/PII checks,
revision consistency, conflict and duplicate detection, then task-specific fixtures.
Static syntax success is not evidence that a procedure is useful. Any model judge has
a recorded version and is labeled as such. Bench execution must use synthetic example
fixtures and existing isolation, with no production credentials or external writes.
Executable candidate scripts are untrusted until the applicable human review.

Runtime activation and repository promotion are separate decisions. Runtime activation
changes the locally executable skill revision; repository promotion accepts a reviewed
proposal into the instance source tree and follows its existing review workflow. Neither
implies the other, a commit, deployment, restart, or permission for external actions.
Activation is a later gated capability. Initial background output remains proposal-only;
patch staging and a compare-and-swap foundation do not authorize automatic activation.
Human review remains required for executable scripts, schedules, capabilities, shared
policy, secrets, external actions, audience widening, and changes to curator protections.
A future owner-enabled automatic lane may cover narrowly defined agent-local procedural
text only after held-out evaluation gates pass. That lane must explicitly list allowed
paths/actions, evidence thresholds, risk classes, maximum changes, and rollback policy.
No inferred opt-in follows from enabling reflection or core automatic lesson review.

Activation acquires the artifact lock, checks expected base and live revision, rechecks
current authority and rejection state, persists a rollback artifact, writes atomically,
refreshes the index, and records before/after hashes plus the decision identity. Crash
recovery reconciles the receipt with the artifact before another activation. A failed
compare-and-swap produces a stale proposal; it must be regenerated or consciously
rebased, never silently overwrite the newer owner edit.

Rollback targets a known revision and also uses compare-and-swap. Do not overwrite a
newer unrelated edit. Negative evidence can pause future automatic activation without
pretending the old revision is now correct. Rejected or revoked revisions remain
blocked through nap, rehydrate, import, and restart.

## Skill revision and maintenance foundation

A revision must cover the complete bounded skill bundle: sorted relative paths, content
hashes, entry type, and executable mode where meaningful. Include `SKILL.md`, scripts,
references, and templates. Hashing only `SKILL.md` misses changed executable behavior.
Exclude operational usage counters and reject symlinks, devices, traversal paths, and
ambiguous duplicate paths. Version the canonical hash algorithm.

Recommended storage interfaces:

```go
Revision(name) (RevisionManifest, error)
PlanMaintenance(inventory, evidence, now) ([]MaintenanceProposal, error)
ApplyRevision(name, expectedBase, expectedLive, candidate, decision) (Receipt, error)
Rollback(name, expectedLive, targetRevision, decision) (Receipt, error)
```

The first foundation produces deterministic inventory and review recommendations.
`Library.PlanMaintenance(MaintenancePolicy)` reads usage plus a private runtime-creation
receipt (`state/needletail/skill-origins-v1.json`) and returns `keep`, `stale`, or
`archive_review`. Repository-owned, generated, pinned, scheduled/protected, ambiguously
named, and legacy skills without matching ownership receipts are held. Missing, corrupt,
or future-dated evidence cannot justify archival. Caller-supplied stale/archive durations
must be positive and ordered; there is no automatic maintenance schedule or archival.
The separate `BundleRevision` and `StagePatch` primitives now provide a bounded
whole-bundle revision check and an immutable unique-match `SKILL.md` patch candidate.
`ListPatches` returns exact root/audience matches. Candidates are stored privately in
`state/needletail/skill-proposals-v1.json` (Raw, 0600), capped at 256 patches and 16 MiB.
`StagePatch` checks generated/read-only protection, schema, secrets, untrusted context,
and the expected bundle revision before and after validation. It does not apply a patch
or constitute a filesystem transaction against arbitrary concurrent terminal writes.
The host must validate root/audience and use the source trust guard when wiring this
primitive into a worker or owner operation. Automatic model patch generation, worker
integration, activation compare-and-swap, and rollback remain subsequent work.
Do not implement a free-running model that rewrites directories. Classify missing
checks, repeated verified failures, expired review dates, obsolete dependencies, broken
references, duplicate procedures, and inactivity separately. Low usage alone is not
proof of obsolescence; a rare recovery skill may be valuable. A dependency freshness
check records when and how it was verified; fetching external sources must use an
approved destination policy and cannot disclose private task content.

Maintain active, candidate, deprecated, archived, and revoked states separately from the
artifact bytes. Deprecation initially changes review metadata only. Archive/delete of a
repository-owned, pinned, generated, shared, safety-related, or curator skill requires
explicit owner review. Preserve source ownership and baseline hashes through restore.
Do not drop a file merely because a newer baseline omitted it without checking existing
rejection, withheld-secret, and tombstone behavior.

## Curator and quarantine protections

The background worker has no terminal, filesystem mutation, messaging, scheduler,
credential, or external network tools. It receives a bounded summary and scoped related
records, not the entire conversation. Treat all supplied text as untrusted data.

The curator cannot edit its own policy, generator/evaluator prompts, budgets, protected
path list, approval records, evidence provenance, quarantine state, capability allowlist,
or activation rules. Host-owned protections must live outside writable skill content;
changing a skill named “curator” cannot change those protections. Only an owner-controlled
configuration path can grant additional authority, and it is auditable.

Validate trust when admitting a job, before retrieval/call, and before persisting a
candidate or export. A root that becomes untrusted while work is queued or running
cannot emit learning or skill mutations. Compacted descendants inherit root quarantine.
Do not clear taint because the worker uses a different session, scheduled run, queue ID,
Luau handler, tool, or restored snapshot. Use existing guarded write boundaries and
filesystem alias protections rather than path-string checks alone.

Audience is immutable provenance, not a field selected by the model. Resolve current
session audience before searching. A private candidate cannot be exported into the
agent-wide dream path. User-profile and PII-flagged knowledge cannot be promoted beyond
the agent; existing stricter export checks remain in force. Public procedural language
does not make private source evidence shareable. Review and queue APIs expose only the
selected authorized scope, including counts where they could leak private activity.

## API and interface roadmap

Extend the existing owner learning-review API additively. Preserve the meaning of
`feedbackSaved`: enqueue failure must not imply feedback was lost. Return job ID and
state for admission; use explicit codes for duplicate, queue full, disabled, budget
exhausted, source unavailable, quarantined, cancelled, and uncertain. Polling status
must not enqueue jobs or call a model. Cancellation is idempotent and version checked.

Browser and macOS interfaces should both show pending/running/terminal work, budget
remaining, last bounded failure reason, source revision, candidate diff and checks,
review due date, explicit export, stale-CAS conflicts, activation receipts, and rollback.
Owner feedback should return promptly after durable admission and refresh candidate data
when work completes. Do not label a queued or generated candidate “learned.” Use the
existing review UI patterns; a backend-only first batch must disclose absent parity.
The core proxies only authenticated owner requests and does not expose private queues
through public fleet status. Update `docs/api.md` and StormoKit decoding when fields land.

## Storage, migration, and compatibility

Before adding a persisted file, record owner, exact path, format version, permissions,
snapshot class, export behavior, size limit, and older-reader behavior. Queue state is
runtime-owned, private 0600, and `Raw`; only deliberately scrubbed eligible exports are
`Learning`. Add new metadata to the engine interface and snapshot fixtures so nap and
rehydrate preserve it without harvesting it into tracked learning.

Create the queue additively. Leave legacy outcomes, reflections, export files, ledger
counters, accepted decisions, and baseline hashes readable. Do not replay old reflection
reservations as new queued jobs or reset the legacy reflection lifetime ceiling. The
additive background worker retains independent explicit cost reservations. Unknown versions
and invalid/oversized documents fail closed without rewrite. Migration is atomic and
idempotent, with an explicit backup and rollback procedure. If hashes change, preserve
legacy identity alongside the versioned full-bundle revision; never treat legacy hashes
as proof that unchanged scripts were checked.

Older runtimes must not overwrite unknown queue state. A downgrade disables new worker
behavior and preserves its files. Nap/restore round-trip tests prove both privacy class
and exact durable reservation preservation. Human decisions and revocations take
precedence over restored candidates.

## Delivery phases and acceptance gates

| Phase | Deliverable | Required gate |
|---|---|---|
| 1 | Durable bounded proposal queue, background worker, correction/failure admission, ownership-aware dry-run maintenance | Restart, budget, concurrency, quarantine, and revision unit/integration tests |
| 2 | Automatic trusted trigger integrations and observable queue controls | No routine-turn calls; dedupe and cancelled/uncertain recovery tests |
| 3 | Deterministic skill maintenance inventory, proposal diffs, protected curator rules | Generated/pinned/shared paths protected; no autonomous live mutation |
| 4 | Evaluation harness and owner review/activation CAS | Exact revision attribution, stale refusal, rollback/crash reconciliation |
| 5 | Browser/macOS parity, retention and operational controls | Synthetic end-to-end owner flows and private-scope denial tests |
| 6 | Optional narrow automatic activation, measured canary rollout | Held-out quality/cost thresholds agreed and met; explicit owner opt-in |

Phase 1 must demonstrate: feedback persists even if enqueue fails; queue admission does
not wait for model latency; queued work survives restart; duplicate feedback and concurrent
workers do not create extra calls; interruption after reservation does not cause another
paid call; malformed state remains unchanged; full capacity stops admission visibly;
shutdown cancels active work safely; null output completes without a candidate; output
cannot change skills or memory; cancellation/quarantine/audience changes cannot race
candidate persistence; and queue snapshots remain Raw after rehydrate.

Skill foundation tests must demonstrate deterministic bundle hashes, script-only changes
invalidating revisions, no symlink/path escape, read-only generated skills, unsupported
hash versions refused, concurrent owner changes preserved, and planning that recommends
review without changing live skill bytes. Later CAS tests must exercise multi-file
activation, crash recovery, rejected-revision restoration, and protected curator paths.

Use synthetic Acme data only. Before committing implementation, run `gofmt -l .`,
`go vet ./...`, and `go test ./...`; run race checks on queue/worker and skill changes.
For API/native changes, run StormoKit tests and the macOS app build. Report the checks
actually run, together with limitations. No real-provider call or deployment is required
to test deterministic queue mechanics.

Held-out evaluation compares memory disabled, current learning, and the candidate on
corrections, recovery, bilingual paraphrases, stale procedures, contradictions, audience
isolation, prompt injection, replay, and rollback. Hard gates are zero unauthorized
promotions, audience leaks, duplicate evidence/cost reservations, or resurrection of
rejected content in fixtures. Report task correctness, repeated errors, false confirmation,
use of stale procedures, latency, token/call cost, and sample sizes. Set numerical quality
and cost thresholds before tuning. Local mechanics passing does not establish a learning
quality gain.

Rollback starts by disabling admission and automatic activation, then stopping the worker.
Preserve evidence, candidates, decisions, and uncertain reservations within retention
policy. Keep existing accepted knowledge and per-turn recall behavior. Inspect any
activation receipt before reverting an artifact; no force overwrite of owner changes.
A deployment, remote restart, or expansion of agent authority remains a separate action.

## First batch validation receipt

Validated locally on 2026-10-27 after GPT-6.1 Sol implementation and cross-review:

- Full `go test ./...` and `go vet ./...` passed.
- Race checks for runtimelearn, skills, and loop passed.
- `gofmt -l .` was empty and `git diff --check` was clean.
- All 95 StormoKit tests passed; the macOS app build succeeded.
- Tests cover actual bounded tool-free model request shape, concurrent reservation
  deduplication, scope/secret/quarantine refusal, corrupted state, uncertain-call
  recovery, rolling budgets, correction and failed-command admission, private
  nap/rehydrate preservation, complete bundle revisions including permission changes,
  staged patch refusal after source changes, and protected dry-run maintenance.

Cross-review found and fixed nonzero terminal exits being treated as successful
execution, foreground locking during auxiliary requests, late quarantine changes
during staging, omitted permission bits in bundle revisions, and candidate size
validation before host provenance was added.

These results validate local mechanics and additive client compatibility. They do
not establish live learning quality, automatic patch generation or activation, new
UI presentation, deployed behavior, or production readiness of unbounded operation.
The worker remains disabled by default; no agent was restarted or opted in.
