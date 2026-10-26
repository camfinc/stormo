# Needletail learning loop improvement plan

Status: implementation and rollout plan, updated 2026-10-26. Batches A–E have an initial implementation in this branch, with deterministic regression coverage. Batch F remains a measured deployment gate; no live fleet rollout or live-model quality claim is included.

## Implementation status

| Workstream | Delivered | Remaining operational work |
|---|---|---|
| Recall | Independent retrieval each turn; smaller candidates considered when a larger one cannot fit; replay tests cover edits, restart, compaction, cancellation, and privacy | Measure task quality and token cost with real workloads |
| Independent evidence | Separate v1 store, exact revisions, idempotent merge, agent-only export, contradiction veto, evidence-aware cache invalidation and retention | Audit evidence collected during a controlled deployment |
| Outcome feedback | Attributed skill commands/views and full memory views; dedicated owner feedback credential and scoped API | Broader attribution remains unknown; define archival operations before reaching capacity |
| Reflection | Disabled-by-default, bounded no-tools model request, trusted persistence, private candidates and explicit procedural export | Enable selectively after cost and quality gates are agreed |
| Evaluation | Deterministic disabled/old/new recall comparison, held-out lexical sentinels, evidence and restore integration tests | Live repeated-error rate, multilingual task correctness, latency, token/cost confidence intervals |

The implementation intentionally fails closed at 4,096 outcome events and 100 lifetime reflection reservations; it does not silently evict evidence. It attributes only explicit foreground skill calls and full memory views, omitting cut hits, symlinked/oversized skill trees, background commands, and uncertain attribution. These are evidence limits, not successful outcomes. See [runtime operation](needletail.md#413-outcome-evidence-and-selective-reflection) and [the recall evaluation](needletail-recall-evaluation.md).

Needletail should retain useful knowledge, retrieve it when needed, and improve procedures from verified outcomes. The current persistence and review pipeline provides a strong foundation, but snapshot repetition and access counts do not establish that a lesson is correct or useful. This plan addresses all five findings from the learning-loop review, starting with recall correctness and evidence quality.

## Scope and existing behavior

The runtime saves hot memories and private archive records, retrieves related records, and lets agents create or modify skills. Stormo snapshots the runtime, folds hot memories into a scrubbed ledger, reviews eligible proposals, and applies accepted material through build and rehydrate.

Relevant implementation:

| Area | Sources |
|---|---|
| Automatic recall and request injection | `internal/needletail/memory/recall.go`, `internal/needletail/loop/loop.go` |
| Record management and ranking | `internal/needletail/memory/manage.go`, `search.go`, `usage.go` |
| Skill activity | `internal/needletail/skills/usage.go` |
| Snapshot harvesting and review eligibility | `pkg/loop/dream.go`, `autoreview.go` |
| Ledger and retention ranking | `pkg/learning/ledger.go`, `compile.go` |
| Review orchestration | `pkg/core/learn.go`, `review.go`, `review_prompt.md` |
| Restore and rejection handling | `pkg/loop/rehydrate.go`, `internal/needletail/memory/merge.go` |
| End-to-end learning checks | `internal/needletail/cli/learning_test.go` |

Preserve these invariants throughout implementation:

- Raw conversations and the private archive do not become bulk input to the dream or reviewer.
- Ledger text is scrubbed. User-profile and PII-flagged entries cannot be promoted beyond the agent.
- Retrieval, evidence, and reflection enforce the originating audience and scope.
- Recall remains bounded background data, never a new instruction authority.
- Review does not grant new runtime capabilities. Executable skill proposals still require human review.
- Existing rejection, tombstone, stale-proposal, snapshot, and restore protections remain effective.
- Persisted format changes require a versioned migration and round-trip tests. Do not reinterpret old counters as new evidence.
- Engine-neutral behavior belongs behind the existing engine interfaces. Use the example instance for tests.

## 1. Make recall reliable across turns

### Problem

`Recaller.Block` remembers recalled IDs for the whole root conversation, including compacted descendants. The injected block is not stored in the transcript. Consequently, a record can be suppressed on later turns even though its text is absent from that turn's context. An edit that preserves the record ID does not invalidate this suppression.

### Implementation

Run relevance selection independently for each user turn. Remove conversation-wide suppression as a correctness gate; retain deduplication within the current block. Compute one block per turn and reuse it across that turn's model requests, preserving the existing stable request behavior.

Re-fetch current record state at the next turn so updates, deletions, expiry, and audience changes apply. A failed or cancelled request must not consume future recall eligibility. Continue to omit recalled text from stored transcripts.

Keep the existing record and character budgets. When a candidate does not fit the remaining budget, try smaller remaining candidates rather than stopping immediately. Do not introduce a cooldown until evaluation establishes that it improves cost without losing necessary context; any future cooldown must account for content revision and actual context availability.

### Acceptance criteria

- The same relevant record is available on two successive turns and after compaction.
- An updated record supplies its new text on the next relevant turn.
- Deleted, expired, or newly inaccessible records are not injected on later turns.
- Cancellation before a model response does not prevent recall on retry.
- Trivial-message gating, scheduled-run defaults, fencing, and audience isolation remain intact.
- Tests inspect the actual model requests and stored transcripts, not only the recall function's output.

## 2. Separate persistence from independent evidence

### Problem

`learning.Upsert` increments `SeenCount` for each distinct nap containing a lesson. Automatic-review eligibility uses that count, and ledger ranking prefers frequently seen entries. An unchanged unsupported note therefore gains weight merely by surviving snapshots.

### Implementation

Preserve `SeenCount` as a persistence count for compatibility, and label it as snapshot sightings in review output. Remove its role as corroboration in new review eligibility and retention policy.

Introduce versioned evidence associated with the exact lesson revision. Evidence must distinguish:

| Signal | Meaning | May establish confirmation? |
|---|---|---|
| Snapshot sighting | Text survived another nap | No |
| Retrieval or view | Content was shown or opened | No |
| Application attempt | A task attempted to use the content | No |
| Verified task result | An independent check supports the relevant claim | Yes, within that check's scope |
| Explicit confirmation or correction | An authorized person confirmed or corrected the claim | Yes, with source and audience retained |
| Model inference | The model believes a lesson is useful | No, by itself |

Deduplicate confirmations by task root and subject revision. Retries, compaction, repeated snapshots, and multiple tool calls in one task must not produce independent confirmations. Distinct task IDs alone are insufficient: record the evidence origin and avoid repeatedly counting the same underlying source.

New proposals without supporting evidence remain available for human review. They must not become eligible for automatic approval solely through age or persistence. Keep existing accepted lessons accepted during migration; mark their evidence as unknown rather than inventing confirmations or retroactively rejecting them.

Retention should preserve pinned entries first, then favor supported, applicable knowledge. Use task confirmation and actual validation time where available; use a deterministic fallback for legacy entries. Snapshot time must not masquerade as fact freshness.

### Acceptance criteria

- Ten naps of one unverified note produce ten sightings and zero confirmations.
- Replaying an event or retrying a task does not increase independent evidence.
- A materially changed lesson does not inherit confirmation of the old text.
- A correction or contradictory result can suspend automatic eligibility pending review.
- Legacy records remain readable, with their existing decisions preserved.
- Review displays distinguish persistence, evidence, and uncertainty.

## 3. Connect learning to task outcomes

### Problem

Skill usage counts include views, file reads, and command invocations. Memory fetches and some writes increment use counters. These measure activity, not whether the information helped complete the task.

### Implementation

Add a bounded, versioned outcome event stream. Keep the existing usage counters for compatibility and operational diagnostics. Start by collecting outcomes in shadow mode, without changing rankings or approvals.

Each event should contain an event ID, task root, subject kind and ID, subject revision, timestamp, audience, source type, execution status, task outcome, and an optional bounded evidence reference. Use separate outcome values for succeeded, failed, corrected, and unknown. A successful tool exit is execution success only; it is not automatically task success.

For skills, identify the exact content hash. For memories, use stable record identity plus content revision. Persist stable event IDs so ingestion, nap replay, and restarts are idempotent. Bound event size and retention; preserve aggregate evidence after raw event retention expires where the applicable privacy policy allows it.

Capture runtime-observable failures directly. Accept stronger success evidence from task-specific assertions or explicit authorized feedback. Keep model-reported success distinguishable and insufficient for automatic confirmation. Leave ambiguous attribution unknown rather than crediting every retrieved record or viewed skill.

Produce a scrubbed review summary containing independent attempts, supported successes, failures, corrections, evidence types, and revision identity. Never include unrestricted transcript excerpts. Change the review cache key to include the proposal revision, evidence summary revision, and review-rule version so new contradictory evidence invalidates an earlier verdict.

### Acceptance criteria

- Viewing a skill is not a successful application.
- A command that exits successfully but fails the task assertion records task failure.
- A failed skill revision does not penalize an unrelated revised implementation.
- Duplicate delivery and restart replay do not duplicate counts.
- Missing feedback remains unknown.
- New negative evidence forces reconsideration of a cached automatic verdict.
- Events and reviewer summaries cannot leak another audience's records or secrets.

## 4. Add selective reflection

### Problem

The dream harvests what the agent already saved into hot memory. It does not itself establish what was learned from a correction, recovered failure, or verified result. Useful lessons can remain only in a conversation, while weak notes can persist.

### Implementation

Add an optional reflection job triggered by an explicit correction, a failed attempt followed by verified recovery, or repeated independently verified use of a procedure. Do not run it after every turn. Deduplicate triggers by task and event, cap calls and tokens, and make an empty result a normal success.

Reflection receives only a bounded, audience-scoped task summary and selected evidence. It has no external mutation tools. Treat untrusted task text as data; reflection cannot turn an untrusted conversation into permission to change skills, schedules, capabilities, or shared policy.

Each candidate must state:

- The proposed fact or procedure and the previous mistake, if relevant.
- Source event references and the checks supporting the claim.
- Conditions under which it applies and known exceptions.
- Audience, sensitivity, and an expiry or review date when time-dependent.
- Whether it proposes a new record, updates an existing record, or supersedes a conflicting one.

Retrieve related records before proposing a write. Exact duplicates are no-ops. Preserve meaningful contradictions for review rather than silently merging them. Do not automatically copy reflection output into hot memory.

Keep private facts in their authorized audience. Only deliberately selected, scrubbed procedural proposals enter the existing review path. Add a narrow proposal interface if necessary; do not pin an archive record merely to make the dream harvest it. The interface must retain the existing user-profile and PII promotion refusals.

### Acceptance criteria

- A corrected mistake produces at most one candidate for the task.
- A recovered failure yields a candidate only when the recovery has supporting evidence.
- A routine turn with no durable lesson produces no write.
- No candidate becomes active policy or a live skill without the applicable review step.
- Candidate generation respects the source audience and existing untrusted-conversation restrictions.
- Disabling reflection stops new jobs without disturbing accepted knowledge.

## 5. Evaluate quality before expanding retrieval

### Evaluation set

Create a deterministic replay suite using synthetic example-instance data. Keep a held-out set separate from examples used to tune thresholds. Include:

| Scenario | Required behavior |
|---|---|
| Repeated question and follow-up | Relevant knowledge remains available on later turns |
| Compaction and restart | Required context can be retrieved again |
| Correction and supersession | Current evidence wins; obsolete claims do not return as authoritative |
| Time-dependent fact | Expired content is excluded from automatic recall |
| Spanish and English paraphrases | Measure misses without assuming lexical retrieval is sufficient |
| Irrelevant lexical overlap | Avoid injecting misleading records |
| Private and shared audiences | No cross-audience disclosure or unauthorized promotion |
| Failed and retried tasks | No false success or duplicate confirmation |
| Rejected proposal and restore | Rejection remains effective through nap and rehydrate |
| Adversarial source text | No instruction escalation through recall, evidence, or reflection |

Compare memory disabled, current behavior, and the candidate implementation using the same tasks, initial stores, and model configuration. Record model and prompt versions. Use deterministic assertions where possible; separately label model-judged outcomes and repeat stochastic runs.

### Metrics and gates

Measure task correctness, repeated-error rate after correction, relevant-memory recall, irrelevant injection rate, obsolete-fact use, false confirmation rate, latency, tokens, and reflection cost. Maintain separate results for each language and audience scenario.

Hard gates are zero audience leaks, unauthorized promotions, duplicate evidence increments, and rejection-resurrection failures in the fixture set. The concrete recall regressions must all pass. For quality and cost metrics, record the baseline and agree numerical thresholds before tuning; report sample sizes and uncertainty rather than claiming improvement from a few examples.

Consider embeddings, query expansion, or hybrid retrieval only after the evaluation identifies specific lexical misses. Any external embedding path requires an explicit data-destination decision, cost budget, and audience-preserving retrieval. It is not a dependency of this plan.

## Delivery sequence

| Batch | Deliverable | Dependency and release condition |
|---|---|---|
| A | Replay fixtures and baseline measurements | Establish quality and cost gates; no production behavior change |
| B | Per-turn recall correction | Recall regression fixtures and privacy checks pass |
| C | Versioned evidence and outcome collection | Migration and replay tests pass; collect in shadow mode |
| D | Evidence-aware review and retention | Shadow evidence audited; legacy behavior documented; review cache invalidation tested |
| E | Selective reflection in proposal-only mode | Outcome evidence available; budget and authority tests pass |
| F | Measured rollout and retrieval tuning | Held-out quality gates pass; expand only for demonstrated misses |

Each batch should be independently reviewable. Add new instance settings through `pkg/instance`, `docs/instances.md`, and `examples/minimal`; preserve existing user values. Keep reflection disabled by default initially. Document any new engine capability and snapshot classification, with migration and `testdata/classify.json` updates where required.

Before changing a persisted schema, specify its owner, location, permissions, snapshot class, version, upgrade behavior, and behavior under an older reader. Prefer a separate versioned evidence store over silently changing the semantics of ledger or usage fields. An older runtime must not erase unknown evidence or misinterpret it as confirmation.

Roll back behavior by disabling reflection and evidence-driven automatic decisions, retaining human review and existing accepted knowledge. Preserve collected evidence for diagnosis within its retention policy. Do not restore snapshot count as proof of correctness. The recall fix should remain unless a specific regression requires reversal.

## Completion checklist

- [x] All five workstreams have initial implementation and regression coverage.
- [x] Existing nap, rehydrate, rejection, scrub, scope, and stale-proposal tests remain green.
- [x] Migration and restart replay tests prove evidence remains readable and idempotent.
- [x] Core review API and browser output distinguish observations from verified outcomes; the native app continues to decode the additive API fields without new evidence controls.
- [ ] Held-out evaluation reports quality, cost, and uncertainty against the baseline.
- [x] Reflection budgets, disable controls, and recovery procedures are documented.
- [x] `gofmt -l .`, `go vet ./...`, and `go test ./...` pass before implementation commits.
- [x] Additive core API fields are documented; StormoKit decoding and integration suite passed on rerun (92 tests).

Review baseline: the memory, CLI, learning, and loop package tests passed during the 2026-10-26 code review. That verifies existing mechanics; it does not establish live-agent learning quality or completion of this plan.

Implementation validation on 2026-10-26: full Go suite and vet passed; `gofmt -l .` and `git diff --check` were clean. Race checks passed for `internal/needletail/runtimelearn` and `pkg/learning`. StormoKit initially failed its file-browser integration scenario, then that scenario and a full 92-test rerun passed. The app tests use the existing local `bin/stormo`; they do not verify a newly packaged app or production runtime. Live model quality, production deployment, and automatic archival remain outside the completed checks.
