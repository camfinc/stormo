---
name: stormo
description: Operate a Stormo agent swarm with the `stormo` CLI. Check and build agents, start, stop or restart them locally or on ECS, review what they learned (naps, the dream, the learnings ledger), inspect an agent, manage secrets and render deployments. Use when the working directory holds a stormo.yaml, or the user mentions Stormo, swarm agents, naps, the dream, learnings to review, the office or the core, or deploying an agent.
---

<!-- installed by stormo {{VERSION}}; `stormo skill install` refreshes this skill -->

# Stormo

Stormo runs an organisation's AI agents. An **instance** is a directory marked by `stormo.yaml`
holding `units/`, `agents/<id>/` (agent.yaml, SOUL.md, skills, schedules), `personas/` and
`bridge/actions.yaml`. The `stormo` binary is the CLI, the core service and the agents' sidecars.

## Find the instance first

`stormo --help` prints the instance it uses (the nearest `stormo.yaml` above the working directory,
or `--instance <dir>`, or `STORMO_INSTANCE`). Read its `stormo.yaml` and `CLAUDE.md`/`AGENTS.md`
if present: an instance's own rules win over this skill.

## Safe to run without asking (read-only)

- `stormo` (status of every agent), `stormo -r` (the same on ECS, read-only)
- `stormo check [agent…]`: validate manifests and compile every agent
- `stormo inspect <agent> [--target aws|local]`: everything the engine computes for an agent, as JSON
- `stormo review [--since 24h]`: fleet health sweep (writes only `.swarm/review/`)
- `stormo logs <agent>`, `stormo learn list <agent>`, `stormo learn skills <agent>`
- `stormo secrets check`: reports which secret names have values, never the values
- `stormo deploy render <agent>`: writes `dist/` and prints aws commands; it never calls AWS
- `stormo bench <agent>` (mock runner), `stormo version`, `stormo core status`

## Ask the user first

- Local lifecycle: `stormo start|stop|restart [agent…]`, `stormo core up|down`. Agents may be live in
  Slack; a restart recycles their home from the latest nap.
- Review decisions are the user's: `stormo learn accept|reject|pin|promote|edit`,
  `learn skill-accept|skill-reject`. Show the proposals; let the user decide.
- `stormo learn` and `stormo dream` rewrite the ledger and proposals in the working tree.
- `stormo secrets init|share` rewrite `secrets.local.yaml`.

## Never without the owner's explicit go, every time

- Anything remote: `-r`/`--remote` with start, stop, restart; `stormo handoff`.
- `stormo secrets push --yes` (writes AWS Secrets Manager).
- Running the aws commands `deploy render` prints, or creating any AWS resource.
- Changing `names:` or `deploy.aws` in `stormo.yaml`: they name deployed services, secrets and nap
  paths, so a change is a migration.

## Hard rules

- Secrets are env var NAMES in manifests; values live only in `secrets.local.yaml` (gitignored,
  0600) or Secrets Manager. Never print, copy or commit a value, and never put a token in a skill,
  script, cron prompt or test.
- `.swarm/` and `dist/` are generated; `.swarm/` can hold raw conversation snapshots (PII). Never
  commit or quote them.
- Never hand-edit `agents/*/learnings/ledger.jsonl`. Use `stormo learn …`; the scrubber and the
  promotion rules (no PII, no user-profile memories beyond the agent) depend on it.
- An agent runs in one place at a time; moving it is `stormo handoff`, never a parallel start.

## Common tasks

- **Health check:** `stormo`, then `stormo review` for logs, cron and nap freshness.
- **Add or change an agent:** edit `agents/<id>/agent.yaml` and `SOUL.md` (schema:
  [references/instances.md](references/instances.md)), run `stormo check <id>`, then
  `stormo inspect <id>` to confirm env, skills and secrets. Starting it is the user's call.
- **Review learnings:** `stormo learn <agent>` (naps → proposals), `stormo learn list <agent>
  --status proposed`, `stormo learn skills <agent>`; summarise them and ask what to accept.
- **Debug an agent:** `stormo logs <agent> -f`, `stormo inspect <agent> --target local`,
  `stormo review <agent>`.

Every command and flag: [references/commands.md](references/commands.md).
