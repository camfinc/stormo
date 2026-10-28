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
agent's change to a repo job (a pause, a new schedule) lasts until the next restart, and the
dream proposes the live jobs, the agent's own and its changes to the repo's, as a schedules
proposal for review. (Needletail never lets the agent change what a repo job runs, §7.)

## 2. When a job runs

- **interval** (`every: 120m` → `{kind: interval, minutes: 120}`): the next run is one interval
  after the last run ended; a job that has never run first runs one interval after it was made or
  the runtime first saw it.
- **cron** (`cron: 0 9 * * 1-5`): five fields (minute hour day-of-month month day-of-week), each
  a list of `*`, `n`, `a-b` with an optional `/step` (`5/10` is 5 to the field's end); month and
  weekday names (`JAN`, `MON-FRI`); weekday 7 is Sunday. When both day fields are restricted a
  day matches either (Vixie cron, croniter); `*` or `?` is unrestricted. Expressions run on a wall
  clock: Hermes' configured zone (`timezone:` in `engine/hermes/config.yaml`); needletail the
  `timezone` of `engine/needletail/config.yaml`, else the container's `TZ` (agent.yaml `env:`),
  else UTC (§8). A time a DST change
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
- A job never overlaps itself: needletail claims a job before anything of its run starts, a tick
  while it runs leaves it alone, and `run` asked several times while it runs gives one more run.
  Needletail runs a job's turn in the scheduled runs' own turn lane (one turn by default,
  `schedules.turns` in `engine/needletail/config.yaml`, 1 to 4), beside the chat's two, so jobs
  and conversations never starve each other. A job with no script takes its slot first: a busy
  lane leaves it due, before anything of it has run, for the next tick or the end of a run,
  whichever comes first. A job's scripts (monitor, pre-run script, a script-only job) run
  first, holding no turn slot, under their own cap of two at a time (a third waits for one): an
  unchanged monitor, an empty script output or a wake gate ends the run with no turn and no slot.
  When the gate says the agent runs and no slot is free, the job stays due, nothing recorded and
  the monitor's new output not stored until the turn holds its slot; it keeps its gate's result
  (the monitor's block, the script's output, the handlers' notes) and takes a slot on a later tick
  without running its scripts or handlers again, until its schedule's next time after it found
  the lane busy, when the result is dropped and the gate runs again on fresh data (the job keeping
  its place in line meanwhile); a result is also dropped when the job is updated, paused or
  removed while its gate runs. A `run` asked meanwhile is that same due run: `run` on a job
  already due (or in line) leaves its due time, and so its place, as it is. Due jobs start the
  longest due first, and a job that found the lane busy keeps its place in line: a job due after
  it (with no script, or whose gate ends first) does not take the lane before it (logged once),
  and every job is compared at its own place, so the line always moves. A `run` asked while a
  run ends is taken in the same step that lets the job go: one more run, never lost and never two.
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
   - same hash: the run is `no_change`, no turn, no delivery (Hermes keeps a short record of it in
     its output dir; needletail keeps none, so its 10 records per job are runs that did something);
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
   "Script Output" block (needletail: at most 16 KB of it, with a note of the size; Hermes puts it
   all). Empty stdout, or the `{"wakeAgent": false}` gate, is a silent run with no
   turn. A failing script does not fail the run: the agent gets a "Script Error" block with the
   failure and reports it.
5. **Handlers** (needletail only): the agent's `on_schedule` handlers whose header's `jobs:`
   names the job (its id or name, or `"*"`), in order (skill dir, then name), each with the job
   (`id`, `name`, `schedule`, `prompt`, `skills`, `deliver`, `repo`), the script's output and the
   monitor's run context (docs/needletail.md §5.5). A handler's `reply` is the run's output, with
   no turn (delivered and recorded as a turn's reply would be; the monitor's change counts as
   handled); `silent` is a silent run; a `pass` may add notes, which go into the prompt as a
   "Handler Notes" block, and the next handler runs. A handler that fails is skipped (logged) and
   the run goes on (a `before_send` one, which the delivery meets, blocks it instead: `delivery_failed`). They are bounded by `limits.script_timeout`, run while the run still holds
   no turn slot when the job has a gate (as its scripts do; a job that then waits for a slot keeps
   their decision with its gate's result, §2), and the link is in the handler, not the job: `cron/jobs.json` and agent.yaml
   do not change, and Hermes, which has no handlers, runs the same job with its prompt.
6. **The prompt**: each of the job's skills loaded as `skill_view` loads it (its SKILL.md, a
   counted view, and needletail makes it the session's current skill so the terminal's `SKILL_DIR`
   resolves), a notice first for skills that were not found; then a short cron hint (the final
   reply is delivered; reply exactly `[SILENT]` when there is nothing new; `[CRON_FAILURE]` to fail
   the run; do not schedule jobs because the task mentions a schedule); the script's block; the
   job's prompt; the monitor's block under "## Run Context".
7. **The turn**: in a fresh session of its own (needletail `cron-<id>-<UTC time>`, source `cron`,
   which the core's hooks show as the platform), ended after the turn. Tools: every tool when
   `enabled_toolsets` is empty, else the tools of each toolset named (`terminal`, `file`
   = read_file, write_file, patch, search_files, `skills` = skills_list, skill_view, skill_manage,
   `memory`, `web` = web_search (with a search provider) and web_extract, …; a tool's own name stands for itself, a toolset the engine has no tool for offers
   nothing). A run that reads the web is marked (information flow): its report is held for the
   owner's approval unless the job delivers only `local`, plus the core's MCP tools unless `no_mcp` is named. A scheduled run is never offered
   the scheduling tool (a job that makes jobs loops) nor `send_message` (its reply is what is
   delivered), nor needletail's `no_reply`. `model`/`provider` pin the job's model (Hermes).

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
  adds a line that the job is worth a review and can be paused. Needletail carries at most 16 KB
  of a failing script's or monitor's output in the notice (and in `last_error`), with a note of its
  size, so a failure is one Slack post; a script-only job's successful output is delivered whole.
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
`agents/<id>/scripts/`, plus any script the agent wrote or edited there: both engines nap
`scripts/**` and bring the agent's own versions back until a reviewer decides, §7 and
ARCHITECTURE.md's learning loop); a path that resolves outside it,
symlinks included, is refused. `cronjob_manage` stores the path relative to `scripts/`, as
agent.yaml names it, whatever form the agent gave. The interpreter comes from the extension, never the shebang:
`.sh`/`.bash` run with bash, anything else with Python (needletail: `python3`; `.luau` in the Luau sandbox, below).
The working directory is the script's own directory (`scripts/` for a script there), the environment the terminal's (secrets scrubbed, the
passthrough allowlist kept), the bound 1 hour (Hermes' default), and the whole process tree is
killed at the end. Needletail runs it, like the terminal, as the agent's own user (uid 10001,
the runtime's group, no capability; docs/needletail.md §7), so a script cannot change
`cron/jobs.json` or the runtime's other files; Hermes runs it as its runtime user. stdout and stderr are trimmed and redacted; a non-zero exit is a failure
described as `Script exited with code N`, then its stderr and stdout.

**`.luau` scripts (needletail only).** A `.luau` script runs in the Luau sandbox
(docs/needletail.md §5), not as a process: no environment, no working directory, the bound
`limits.script_timeout` (30 s by default; a lower `timeout:` in its header wins), not the hour.
It must open with the `--[[needletail … ]]` header (`format: 1` and the `caps:` it uses; name and
description are optional), and it gets the effective caps (declared ∩ granted) like a tool. Its
output is what it printed (`print`), then what its chunk returned: a string as it is, any other
value as JSON (`nil` adds nothing). The rules on output are the same as a process's: empty or
`{"wakeAgent": false}` is silent, a monitor's output is hashed. A script that raises an error, is
refused a call it did not catch, or passes its bound fails, described as `Script failed:
<code>: <message>`, then its `nt.log` lines and its output. Hermes has no `.luau` scripts.

## 6. Delivery

`deliver` (and `failure_deliver`) is a comma-separated list of targets:

| Target | Means |
|---|---|
| empty, `local` | nowhere: the run is only recorded |
| `origin` | the chat the job was made from (its `origin`), else the home channel |
| `slack`, `home`, `all` | the home channel (`SLACK_HOME_CHANNEL`) |
| `slack:<channel>`, `slack:<channel>:<thread>` | that channel, or a thread in it |

Hermes also delivers to its other platforms and Bot Chat. Needletail delivers to Slack only,
through its Slack gateway (docs/needletail.md §4.9): a user id as the channel is that user's DM;
`origin` for a job made in a DM is the DM itself (needletail keeps one conversation per DM); the
output is posted as it is (Hermes wraps it in a "Cronjob Response" header unless
`cron.wrap_response: false`), with every `@channel`, `@here`, `@everyone` and user group escaped;
a Slack error is the run's `delivery_failed`. Another platform's target is a delivery error. An
agent with no Slack gateway running accepts a Slack target and only logs it (the output stays in
the run's record).

**Whose targets are trusted (needletail).** A job agent.yaml defines is trusted: its targets are
the repo's (needletail.yaml `schedules.repo` lists the repo's job ids; a job with no `created_at`
is the repo's render too). A job the agent made delivers only where the runtime's messaging
policy lets it (docs/needletail.md §4.9: the home channel, a channel or thread the agent is in,
an allowed user's DM), as `send_message` does: `cronjob_manage` refuses a `deliver` or
`failure_deliver` outside it, and each delivery checks it again (a refused target is the run's
`delivery_failed`). Hermes has no such check.

## 7. The agent's own jobs

The agent manages jobs with `cronjob_manage` (Hermes' tool, toolset `cronjob`): `list`, `create`
(`schedule`, `prompt` unless `no_agent`, and optionally `name`, `skills`, `script`, `monitor`,
`no_agent`, `repeat`, `deliver` (default `origin`), `failure_deliver`, `enabled_toolsets`,
`paused`), `update` (the same fields; `""` and `[]` clear), `pause` (`reason`), `resume`,
`remove` and `run` (due now). A job agent.yaml defines delivers to its targets as the repo's
(§6), so needletail refuses an `update` of what it runs or how a handler names it (`prompt`,
`skills`, `script`, `monitor`, `no_agent`, `enabled_toolsets`, `name`), pointing the agent at a
job of its own (which the dream proposes for review) or a person changing agent.yaml; its
`schedule`, `repeat`, `deliver` and `failure_deliver` (held to the policy) can change, and it can
be paused, resumed, removed and run. Schedules: `30m`, `every 2h`, `every hour`, `every monday 9am`,
`weekdays at 9:30am`, `every day at 14:00`, or a cron expression. A job the agent makes is a job
like the repo's; across restarts it follows §1. A script the agent writes in `scripts/` for it
(subdirectories included, caches not) is napped with it, comes back at every restart until a
reviewer decides, and goes to review with the job (the schedules proposal's scripts): accepting
puts it in `agents/<id>/scripts/`, rejecting keeps that version out. An edit to a repo script
comes back the same way, unless the repo changed that script since the instance booted.

The prompt given to `create` and `update` is scanned as Hermes scans it (its strict cron prompt
scan): an injection directive ("ignore previous instructions", "do not tell the user", …), a
command that reads secrets (`cat …/.env`, SSH keys), a backdoor (`authorized_keys`, sudoers),
`rm -rf /`, a `curl`/`wget` that sends a credential-looking variable (`$…KEY`, `$…TOKEN`, …) in a
URL, body or `Authorization` header (a GitHub token to `api.github.com` exactly is allowed), and
zero-width or bidirectional control characters (a joiner inside an emoji is fine) are refused.
Needletail also bounds the agent's own jobs (Hermes does not): at most 20 (jobs with a
`created_at`, which the repo's render never sets, and not among the repo's jobs, so a job
promoted from the agent's into agent.yaml no longer counts), a prompt of at most 8 KB, and no
schedule that runs more often than every 5 minutes (an interval under 5 minutes, or a cron
expression with two runs less than 5 minutes apart, measured from its first run on, however far
away: `* * 15 * *` runs every minute of the 15th and is refused; in elapsed time on the agent's
wall clock, around every daylight-saving change of the 28 years ahead, the calendar's weekday
cycle: in Europe/Rome `0,59 1,3 * * *` runs at 01:59 and, a minute later, at 03:00 on the last
Sunday of March, and `2,58 1,3 31 3 *` four minutes apart in 2030, when March 31st is that
Sunday). The `deliver` and `failure_deliver` it sets meet the messaging policy (§6), `origin` read
as the chat the job was made from (else the home channel), as its deliveries read it; so does the
default `origin` of a job created with no `deliver` (a job made in a DM the agent may not message
is refused at once). The targets are checked before `cron/jobs.json`'s lock is taken (the policy
may ask Slack whose DM a channel is). `cron/**` is not written by the file
tools, nor, with needletail's uid split (docs/needletail.md §7), by the terminal or a job's
scripts, which run as another user than the runtime: these bounds hold for everything the agent
does. (Started without the split, or on Hermes, the terminal can still edit `cron/jobs.json`.)

## 8. Where needletail differs (M4)

- **Timezone.** The agent's wall clock is `timezone:` in `agents/<id>/engine/needletail/config.yaml`
  (an IANA name, checked at build and rendered into `needletail.yaml`), else `TZ` in the
  container's env (agent.yaml `env: {TZ: …}`), else UTC. Cron expressions and the system prompt's
  "current time" use it; the times in `cron/jobs.json` carry its offset. To move a Hermes agent,
  copy the `timezone:` line of its `engine/hermes/config.yaml` into
  `engine/needletail/config.yaml`; the stamps Hermes left in `cron/jobs.json` carry its offset and
  keep their meaning (a cron time whose own wall clock matches is kept, §1). An image older than
  this key refuses a `needletail.yaml` that has it (the file's keys are checked), so a timezone
  needs this runtime or later.

- **Handlers** (M6b). The agent's `on_schedule` handlers may answer, silence or annotate a run
  before its turn (§3, step 5). Hermes has none.
- **Users.** Scripts and monitors run as the agent's user, apart from the runtime (§5).

- One-time schedules (`in 30m`, a timestamp) are refused: agent.yaml has no form for them, so the
  dream could not propose them. Something done once, later, is a **follow-up**, not a job
  (docs/needletail.md §4.12): the `follow_up` tool (and Luau's `follow_up`) sends a text or takes a
  turn once, 5 seconds to 30 days ahead, in the conversation it was asked in (or another target the
  messaging policy allows). Follow-ups are not in `cron/jobs.json`, never reach review, are kept in
  the runtime's private `state/needletail/followups.json` (so a pending one survives a restart; one
  missed while the agent was down fires at the next boot) and are never offered to a scheduled run,
  which has no conversation to come back to. The refusal points the agent at `follow_up`, and so
  does the 5-minute floor (§7). A prompt follow-up's turn takes the scheduled runs' lane (§2).
- `model` and `provider` are recorded but not applied: every job runs on the agent's model (logged
  once per job).
- Not implemented: `monitor_url`, `context_from` and `continuity`, `workdir`, the per-job notepad,
  `attach_to_session`, the prompt-injection scan of the assembled prompt, the automatic re-run of a
  run that never reached the model, quota holds, Hermes' other platforms.
- A Slack origin thread in the home channel is kept (Hermes drops it as stale), and a delivery is
  not wrapped in Hermes' "Cronjob Response" header.
