# Scheduled report delivery recovery

Execution and delivery are separate outcomes. A successful research run can produce an
undelivered report. Source deduplication or `mark-analyzed` must never be interpreted as a
receipt from the destination. The scheduler now retains the final report before attempting
external delivery; subsequent executions of that job wait for recovery or explicit discard.
This prevents a later empty refresh from replacing the report that failed to reach the owner.

## Runtime contract

- Persist the exact final reply and per-target acknowledgement before advancing recovery.
  Local-only and silent runs do not create an outbox entry.
- Reports waiting for review (their five attempts spent, or a permanent error) share ONE notice
  in the configured Slack home: a count and the jobs' opaque ids, never report text, a
  destination, a job name or a provider error. It is posted for the first report, updated in
  place (`chat.update`) as the set changes, and updated to say delivery recovered when none
  waits. It bypasses model and before-send handlers and never broadcasts. A failed notice is
  tried at most three times per set of jobs. The report itself is never rerouted implicitly.
- Transient failures retry the saved report on scheduler ticks, with exponential delays of
  2, 4, 8, 16 minutes and at most five attempts including the initial send; no notice before
  they are spent. A policy refusal, an unsupported platform, a missing channel or membership
  and an auth error are permanent: one attempt, then review. Each kept report carries a coarse
  `error_class` (`policy_refused`, `unsupported_platform`, `not_in_channel`, `auth`,
  `transient`). This does not assert that a channel is invalid: Slack may conceal a channel
  the bot cannot access.
- A shutdown or cancellation during a delivery is not a failure: no attempt is counted and the
  report goes at the next start. Recovery does not run while the scheduler stops, and a tick
  never waits on a delivery under way.
- Successful destinations are not resent.
- `cronjob_manage list` exposes `last_delivery_error` and `delivery_recovery` metadata:
  pending, attempts, needs_review, home_alert_sent, quarantined, error_class. It never exposes
  retained text.
- `retry_delivery` with `job_id` queues the saved report without running research again, and
  counts it for the notice again should it fail again. An optional `deliver` explicitly
  replaces unsent destinations under the current messaging policy, and only within the
  report's original audience (its targets' channels, any thread), the home channel or an
  approver's DM; it does not edit the job's future destination. Use `update` separately for
  that. From a tainted run, `retry_delivery` and `discard_delivery` need the owner's approval.
  Completed one-shot jobs can still recover delivery. A queued result is not a send receipt.
- `discard_delivery` explicitly abandons the pending report and releases the execution hold.
  Normal `run` and `remove` refuse a pending report. Pausing execution does not erase the report
  or cancel already pending transport recovery.
- A tainted run's report (its run took in marked data, docs/needletail.md §7) never enters this
  outbox: it is held as an approval card with exactly the report and its targets; Approve sends
  it, Deny or no decision in `approvals.delivery_expiry_days` (3) drops it; the job keeps its
  schedule and its `last_delivery_error` names the request. A job that delivers only `local`
  holds nothing (its run record is the delivery). The outbox's `quarantined` flag remains for
  entries made before (they need private review and explicit discard).

## Learning and reflection

Verified delivery failure and recovery produce private, conversation-scoped outcome evidence
with opaque report/job identities. They do not confirm a skill, a lesson, or the correctness of
research. Recovery records successful transport with the original task outcome still unknown.
The original execution failure flag is retained, so successfully sending an error report does
not turn the failed job into a successful task.

No model call, automatic lesson promotion, or reflection is required to retain, retry or alert.
Reflection remains optional. A human can review the failure and confirmed fix before proposing
a reusable procedural lesson; ambiguous provider errors are not facts about the destination.
Missing/quarantined source sessions skip evidence. Evidence-store errors are logged without
blocking operational recovery. There is no cross-store transaction, so a crash may omit an
evidence observation; delivery receipts remain the operational authority.

## Persistence and migration

`state/needletail/deliveries-v1.json` is a version-1 private outbox, mode 0600, classified
Raw in snapshots (pinned in classify.json). The `alert` (the one notice), `error_class` and
`audience` keys are additive; an older engine ignores them (and alerts per report). Writes use the existing fsynced atomic-rename helper and excluded temporary
directory. Missing file means empty; malformed or unknown versions fail closed and block new
scheduled execution. Counters and entry identity are validated. Maximum 1000 pending reports;
no eviction of undelivered content. It is single-writer within the running scheduler, like jobs.

`cron/jobs.json` receives additive `delivery_hold` and `last_execution_status` fields. Missing
fields preserve prior behavior. Older engines do not honor the new delivery hold: drain or
explicitly discard the outbox before rolling back. Existing historical `delivery_failed` jobs
are not automatically imported or replayed; an operator must inspect their private retained
run records and confirm the destination first. Never delete source deduplication state to
recover a missing report.

Per-target acknowledgement gives at-least-once delivery. A crash between provider acceptance
and local acknowledgement may duplicate a message or alert. An acknowledgement means the
provider accepted the post, not that a person read it. A persistence failure stops sending;
if the report could not enter the outbox, the job remains held and its private run record must
be reviewed before discard/rerun. No claim of exactly-once delivery is made.

## Validation and rollout

Tests use fake delivery adapters and a fake Slack HTTP endpoint: partial success, restart,
explicit repair, policy rejection, bounded retries, quarantine, corrupt storage, persistence
failure, home routing, evidence privacy and replay. No live Slack messages are part of tests.
Build/commit is not deployment. Roll out a pinned engine only after the normal review gates,
confirm the configured report destination and home, then exercise one authorized recovery.
