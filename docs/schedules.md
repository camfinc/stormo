# Schedules

How an agent's scheduled jobs behave, whichever engine runs them. Hermes (the image's `cron/`
package, read at `v2026.9.24`) is the reference; needletail (`internal/needletail/sched`)
implements the same semantics, and the places where it does not yet are listed at the end. An
agent's jobs come from `schedules:` in agent.yaml (docs/agent-standard.md) and from the agent
itself at runtime.

## 1. The file

Both engines keep jobs in `cron/jobs.json` in the agent's home: `{"jobs": [ … ]}` (a bare list
is read too). The adapter renders it from agent.yaml at build time; the runtime is its only
writer while the agent runs, and writes it atomically.

A job record has **definition** keys (what agent.yaml decides) and **run state**:

| | Keys |
|---|---|
| Definition | `id`, `name`, `prompt`, `skills` (and the legacy `skill`), `model`, `provider`, `base_url`, `script`, `no_agent`, `monitor_script`, `monitor_url`, `context_from`, `schedule {kind: interval\|cron, minutes\|expr, display}`, `schedule_display`, `enabled`, `state`, `paused_reason`, `deliver`, `failure_deliver`, `enabled_toolsets`, `workdir`, `repeat.times` |
| Run state | `next_run_at`, `last_run_at`, `last_status`, `last_error`, `last_delivery_error`, `failure_streak`, `repeat.completed`, `monitor_state {last_output_hash, last_changed_at}`, `paused_at`, `created_at`, `origin`, and needletail's `running` |

Keys an engine does not model are kept as they are on every write.

**Across a restart** (rehydrate, `Engine.MergeSchedules`): for a job in both the repo's render and
the napped file, the repo's definition wins and the napped run state is kept; a job only the
napped file has (one the agent made) is kept whole; a job a reviewer rejected is dropped. When
the repo changed a job's schedule, its kept `next_run_at` may not follow the new one: a future
time the new schedule would not give is re-anchored to the schedule's next time without firing
(Hermes' `_reanchor_stale_cron`; needletail does it for intervals too, when the stored time is more
than one interval away), while a past one fires once. A cron time whose own wall clock matches the
expression (stamped in another zone's offset) is kept. A job whose `repeat.times` runs are done
stays done even though the merge gives it the repo's `enabled` and `state` back. So an
agent's change to a repo job (a pause, a new prompt) lasts until the next restart, and the
dream proposes the live jobs, the agent's own and its changes to the repo's, as a schedules
proposal for review.

## 2. When a job runs

- **interval** (`every: 120m` → `{kind: interval, minutes: 120}`): the next run is one interval
  after the last run ended; a job that has never run first runs one interval after it was made or
  the runtime first saw it.
- **cron** (`cron: 0 9 * * 1-5`): five fields (minute hour day-of-month month day-of-week), each
  a list of `*`, `n`, `a-b` with an optional `/step` (`5/10` is 5 to the field's end); month and
  weekday names (`JAN`, `MON-FRI`); weekday 7 is Sunday. When both day fields are restricted a
  day matches either (Vixie cron, croniter); `*` or `?` is unrestricted. Expressions run on a wall
  clock: Hermes' configured zone; needletail the container's (`TZ`, else UTC). A time a DST change
  skips does not run that day.
- The scheduler ticks (needletail every 30 s). A due job whose time has passed runs **once**, even
  when several of its times passed while the agent was down: the backlog collapses into one run,
  and the next time is computed from then.
- `next_run_at` moves to the next time **before** the run starts, and the job is marked running, so
  a crash mid-run never runs it twice. A run a crash cut short is recorded as a failure at boot.
- `run` on a job that is running makes it due again as soon as that run ends (needletail keeps
  the request in memory, so a restart forgets it).
- A run the runtime's shutdown cuts short is **interrupted** (needletail): nothing is delivered,
  the failure streak, `last_status` and `repeat.completed` stay as they were, and the job is due
  again at the next start. A run still marked running at boot (a crash, a kill) is recorded as
  failed.
- A job never overlaps itself. Needletail runs an agent job only when it gets a turn slot (the
  API's cap of two turns): a busy agent leaves the job due for the next tick, before anything of it
  (monitor, script) has run.
- `enabled: false` (`state: paused`) and `state: completed` jobs do not run.
- A job whose next time cannot be computed (a schedule the engine does not run, such as
  needletail's `@hourly`, `mon#1`, `L` or a one-time `once` kind; an interval over 366 days; an
  expression that never matches) gets no `next_run_at`, `state: error` and the reason in
  `last_error`, and does not run (Hermes does the same). It stays enabled: the scheduler checks
  its schedule again on every tick (needletail checks every job's schedule, even one with a stored
  time) and schedules it again once it works. Intervals are at most 366 days.

## 3. A run

In this order:

1. **Script-only jobs** (`no_agent: true`, agent.yaml `agent: false`): the script runs and is the
   whole job (§5). A failure delivers an alert (`⚠ … script failed` and its output); stdout that
   is empty, or whose last non-empty line is JSON `{"wakeAgent": false}`, is a silent run;
   anything else is delivered as it is. A monitor is not used with `no_agent`.
2. **Unrunnable jobs** are paused, not retried every tick: a `no_agent` job with no script, an
   agent job with no prompt, skills or script. `paused_reason` says why
   (`Auto-paused by the scheduler: …`) and an alert goes out. Nothing else ever pauses a job: a
   failure streak only adds a nudge (§4).
3. **Monitor** (`monitor_script`; Hermes also `monitor_url`): runs first and decides whether the
   agent runs at all. Its exact output (trimmed stdout, no other normalisation: it must be
   deterministic, no timestamps) is hashed against `monitor_state.last_output_hash`:
   - same hash: the run is `no_change`, no turn, no delivery, no record;
   - first run (no hash yet): the agent runs with a "Monitor Baseline (first run)" block holding
     the current output;
   - changed: the agent runs with a "MONITOR CHANGE DETECTED" block: a unified diff against the
     previous output (`--- previous`/`+++ current`, 3 lines of context, at most 4000 characters)
     and the current output (at most 8000);
   - the new hash and output are stored **before** the agent runs, so a failed run does not alert
     on the same output again (needletail keeps the output in
     `state/needletail/monitor/<id>.out`, Hermes in its cron output dir);
   - a monitor that fails is the run's error (an alert, `last_status: error`) and leaves the stored
     hash as it was.
4. **Pre-run script** (`script` on an agent job): its stdout goes into the prompt as a
   "Script Output" block. Empty stdout, or the `{"wakeAgent": false}` gate, is a silent run with no
   turn. A failing script does not fail the run: the agent gets a "Script Error" block with the
   failure and reports it.
5. **The prompt**: each of the job's skills loaded as `skill_view` loads it (its SKILL.md, a
   counted view, and needletail makes it the session's current skill so the terminal's `SKILL_DIR`
   resolves), a notice first for skills that were not found; then a short cron hint (the final
   reply is delivered; reply exactly `[SILENT]` when there is nothing new; `[CRON_FAILURE]` to fail
   the run; do not schedule jobs because the task mentions a schedule); the script's block; the
   job's prompt; the monitor's block under "## Run Context".
6. **The turn**: in a fresh session of its own (needletail `cron-<id>-<UTC time>`, source `cron`,
   which the core's hooks show as the platform), ended after the turn. Tools: every tool when
   `enabled_toolsets` is empty, else the tools of each toolset named (`terminal`, `file`
   = read_file, write_file, patch, search_files, `skills` = skills_list, skill_view, skill_manage,
   `memory`, …; a tool's own name stands for itself, a toolset the engine has no tool for offers
   nothing), plus the core's MCP tools unless `no_mcp` is named. A scheduled run is never offered
   the scheduling tool (a job that makes jobs loops) nor `send_message` (its reply is what is
   delivered). `model`/`provider` pin the job's model (Hermes).

## 4. The outcome

| `last_status` | When | Delivered | `failure_streak` |
|---|---|---|---|
| `ok` | the run answered (or a script-only job printed) | the reply / stdout | reset to 0 |
| `silent` | a `[SILENT]` reply, an empty script output, a wake gate | nothing | reset to 0 |
| `no_change` | the monitor's output did not change | nothing | reset to 0 |
| `error` | the turn failed, a `[CRON_FAILURE]` reply, an empty reply, a failing monitor or script-only script, an unrunnable job | a failure notice | one more |
| `delivery_failed` | the run worked but a target refused its output | (it was attempted) | unchanged |

Hermes (v2026.9.24) records silent and unchanged runs as `ok` (its statuses are `ok`, `error` and
`delivery_failed`, plus `blocked_config` and `delivery_queued`); `silent` and `no_change` are
needletail's finer names for them, which the adapters read as successes. `stormo review` (Engine.Schedules)
flags a job whose last status is none of `ok`, `silent`, `no_change`, a failure streak, a delivery
error, and a job overdue by more than 15 minutes.

- **Silence**: `[SILENT]` (or `SILENT`, `NO_REPLY`, `NO REPLY`), any case, as the whole reply or as
  its own first or last line. The token inside a sentence is content, delivered.
- **Agent-declared failure**: `[CRON_FAILURE]` alone on the reply's first line; the lines after it
  are the error.
- **Failure notices** go to `failure_deliver` when the job has one, else to `deliver`
  (`failure_deliver: local` keeps them out of chat). From the third failure in a row the notice
  adds a line that the job is worth a review and can be paused.
- `last_error` is cleared by a success; `last_delivery_error` is set by each run's delivery.
- `repeat.completed` counts every run; with `repeat.times` set, the run that reaches it completes
  the job (`enabled: false`, `state: completed`, no next run).
- **Pause** sets `enabled: false`, `state: paused`, `paused_at` and `paused_reason`. **Resume**
  clears them; a time that passed while the job was paused stays due (one run on the next tick, no
  replay); otherwise the next time is computed afresh.
- Each run that did something is recorded (Hermes: its cron output dir; needletail:
  `state/needletail/jobs/<id>/<time>.md`, the newest 10 per job), whatever its delivery.

## 5. Scripts

A job's `script` and `monitor_script` are files in the home's `scripts/` (the agent's
`agents/<id>/scripts/`, rebuilt from the repo at every boot); a path that resolves outside it,
symlinks included, is refused. The interpreter comes from the extension, never the shebang:
`.sh`/`.bash` run with bash, anything else with Python (needletail: `python3`; `.luau` from M6 on).
The working directory is the script's own directory (`scripts/` for a script there), the environment the terminal's (secrets scrubbed, the
passthrough allowlist kept), the bound 1 hour (Hermes' default), and the whole process tree is
killed at the end. stdout and stderr are trimmed and redacted; a non-zero exit is a failure
described as `Script exited with code N`, then its stderr and stdout.

## 6. Delivery

`deliver` (and `failure_deliver`) is a comma-separated list of targets:

| Target | Means |
|---|---|
| empty, `local` | nowhere: the run is only recorded |
| `origin` | the chat the job was made from (its `origin`), else the home channel |
| `slack`, `home`, `all` | the home channel (`SLACK_HOME_CHANNEL`) |
| `slack:<channel>`, `slack:<channel>:<thread>` | that channel, or a thread in it |

Hermes also delivers to its other platforms and Bot Chat. Needletail delivers to Slack only (M5);
another platform's target is a delivery error.

## 7. The agent's own jobs

The agent manages jobs with `cronjob_manage` (Hermes' tool, toolset `cronjob`): `list`, `create`
(`schedule`, `prompt` unless `no_agent`, and optionally `name`, `skills`, `script`, `monitor`,
`no_agent`, `repeat`, `deliver` (default `origin`), `failure_deliver`, `enabled_toolsets`,
`paused`), `update` (the same fields; `""` and `[]` clear), `pause` (`reason`), `resume`,
`remove` and `run` (due now). Schedules: `30m`, `every 2h`, `every hour`, `every monday 9am`,
`weekdays at 9:30am`, `every day at 14:00`, or a cron expression. A job the agent makes is a job
like the repo's; across restarts it follows §1.

## 8. Where needletail differs (M4)

- One-time schedules (`in 30m`, a timestamp) are refused: agent.yaml has no form for them, so the
  dream could not propose them.
- `model` and `provider` are recorded but not applied: every job runs on the agent's model (logged
  once per job).
- Not implemented: `monitor_url`, `context_from` and `continuity`, `workdir`, the per-job notepad,
  `attach_to_session`, the prompt-injection scan of the assembled prompt, the automatic re-run of a
  run that never reached the model, quota holds, Hermes' other platforms.
- Slack delivery waits for the Slack gateway (M5): until then a Slack target is accepted and
  logged, and the output stays in the run's record.
