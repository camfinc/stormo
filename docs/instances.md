# Instances

Stormo is the engine. An **instance** is one organisation's swarm: a portable directory holding its
agents, units, personas, bridge actions and policy docs, marked by `stormo.yaml` at its root. The
engine never names an organisation; everything org-specific lives in the instance, so the same
engine checkout serves any number of instances, and an instance can move between machines, repos
or engine versions without touching engine code.

[`examples/minimal/`](../examples/minimal) is a complete instance (made-up org "Acme", two units,
three agents). It is the engine's test fixture and the template for a new one.

## Layout

```
<instance>/
  stormo.yaml                    required: marks the instance (schema below)
  units/<unit>/unit.yaml         required: at least `group` plus one unit
  units/<unit>/knowledge/*.md    human-authored policy compiled into that layer's agents
  agents/<id>/agent.yaml         one directory per agent (manifest: ARCHITECTURE.md "Layout")
  agents/<id>/SOUL.md            behavior
  agents/<id>/{skills,plugins,scripts,bench,<engine>}/   optional, as in ARCHITECTURE.md
  agents/<id>/learnings/         written by the dream; reviewed in git
  (`stormo export <agent> [--data]` packs the folder, its persona, its unit's identity and the
  bridge actions it uses into one zip; `--data` adds its naps and secret values. `stormo import`
  unpacks one, renamed with --as or into another unit with --unit, and keeps it only if it checks.)
  agents/<id>/data/              the agent's data, never in git (its own .gitignore): store/ (local
                                 naps), secrets.yaml (its own secret values; shared and unit values
                                 stay in secrets.local.yaml), agent.env (rendered for local runs)
  personas/<slug>/               optional: look + baked avatar (agent.yaml `persona.path`)
  bridge/actions.yaml            optional: the instance's bridge actions (stormo.yaml `bridge.actions`)
  tests/                         optional: the instance's own tests (see "Tests")
  docs/, CLAUDE.md, README.md    the instance's own docs and rules
  go.mod                         when it has Go tests against the engine (see "Tests")

  # generated, gitignored, never leave the machine
  dist/                          compiled baselines, task definitions, Slack manifests
  .swarm/                        local runtime: nap store, compose, env files, core auth and core.db (PII)
  workdir/                       local shared space: workdir/group, workdir/<unit> (agents' documents, can hold client data)
  secrets.local.yaml             every secret value, 0600
```

Runtime state belongs to the instance, not the engine: moving an instance moves its naps, ports
and local secrets with it, and two instances never share a store.

## `stormo.yaml`

```yaml
name: Acme Swarm          # display name: CLI, office UI, the ChatGPT sign-in page
org: Acme                 # organisation name in text compiled into agents ("shared across all of Acme")
slug: acme                # required, lowercase; the default for every name below
author: Acme Inc          # author of generated engine distributions (default: org)

names:                    # PINNED once anything is deployed: see below
  resource: acme-stormo           # ECS family/service, /ecs/ log group, sidecar image, secret tags
  secret: acme/stormo             # Secrets Manager id prefix: <secret>/<agent>
  state_dir: acme-state           # engine-home dir for manifest `state:` (nap rules follow it)
  knowledge_skill: acme-knowledge # generated skill the agents load by name
  shared_skill: acme-shared-docs  # generated shared-documents skill

scrub:
  token_prefixes: [acme]  # `<prefix>_…` tokens the PII scrubber redacts, on top of the engine's

bridge:
  actions: bridge/actions.yaml    # the instance's bridge actions (see "Plug-in points")

office:
  clocks:                         # lobby wall of the office UI
    - { city: Lisbon, tz: Europe/Lisbon }

core:
  learning:                       # the swarm core's learning cycle (docs/core.md §6); off without `at`
    at: "03:00"                   # daily start, 24-hour time; each agent is napped, then dreamed
    timezone: Europe/Lisbon       # for `at` (default: the host's)
    stagger_minutes: 2            # between agents in a scheduled cycle (default 2)
    quiet_wait_minutes: 30        # wait this long for a busy agent, then dream without a fresh nap (default 30)
    agents: [atlas]               # default: every agent

deploy:
  aws:                            # unset fields render as <unset> and `deploy render` reports them
    account: "123456789012"
    region: us-east-2
    cluster: acme-ecs
    bucket: acme-stormo-naps
    registry: ghcr.io/acme
    execution_role_arn: arn:aws:iam::123456789012:role/ecsTaskExecutionRole
    task_role_arn: arn:aws:iam::123456789012:role/acme-stormo-task
    efs: { file_system_id: fs-…, access_points: { group: fsap-…, sales: fsap-… } }
```

**Pinned names.** Everything under `names:` ends up somewhere that outlives a deploy: ECS service
and task family names, Secrets Manager ids, the `state_dir` the nap rules snapshot, and skill names
the agents' own SOULs and skills refer to. Write them out explicitly once an instance has deployed
anything, and keep a test that asserts them. Renaming one
is a migration, not an edit. `name`, `org`, `author` and `office` are cosmetic and safe to change.

## How the engine finds the instance

First match wins:

1. `--instance <dir>` on any command;
2. `STORMO_INSTANCE`;
3. the nearest `stormo.yaml` at or above the working directory;
4. the nearest at or above the `stormo` binary itself (a binary built inside its instance's
   `stormo/` checkout finds that instance from any directory).

Commands that need no instance (`stormo --help`, `stormo version`) run anywhere; every other one
stops with an error rather than guessing, since an instance's names decide nap paths and secret ids.

Commands print the instance they use (`stormo --help` shows it). `SWARM_HOME` is not the
instance: inside a sidecar it is the agent's engine home.

## Plug-in points

- **Bridge actions.** `bridge.actions` names a YAML file (relative to the instance) listing the
  instance's actions. Each is owned by a unit or `group` (the first segment of its name), and
  `stormo check` refuses an agent listing an action of another unit. Only `check` and the bridge
  read it, never the sidecars:

  ```yaml
  - name: sales.crm.deals.list          # <unit|group>.<system>.<resource>.<verb>
    description: List deals from the CRM (read-only).
    inputSchema: {type: object, properties: {dateFrom: {type: string}}, additionalProperties: false}
    secrets: [CRM_API_TOKEN]            # held by the bridge, not the agent
    mutates: false                      # true needs an explicit approval path
    http:
      method: GET
      url: https://crm.example.com/api/deals
      auth: {bearer_secret: CRM_API_TOKEN}
      query: from_input                 # non-empty input fields become the query string
  ```

- **Personas.** `persona: {path: personas/<slug>}` resolves inside the instance; add
  `repo: <dir>` for a persona kept in a sibling repo (resolved against `SWARM_REPOS_DIR`, default
  the instance's parent).
- **Engines.** Agents pick `engine.kind`; what only that engine reads lives in `agents/<id>/engine/<kind>/`
  (format 0: `agents/<id>/<engine>/`). agent.yaml itself is engine-neutral (format 1,
  docs/agent-standard.md); `stormo migrate agent` brings a format-0 agent there.

## Tests

The engine's suite (`cd stormo && go test ./...`) runs on `examples/minimal` only, so it passes
without any real instance. An instance checks its own agents two ways, pick either:

- **Black box, any language.** `stormo inspect <agent> [--target aws|local]` prints everything the
  engine computes for an agent as JSON: the manifest as it runs, container env, file and skill
  hashes, the parsed engine config, the task definition and policy. Assert on it with whatever
  the instance already uses (`jq`, a shell script, a test runner).
- **Go tests against the engine's packages.** Make the instance a Go module that points at the
  vendored engine, and import `github.com/camfinc/stormo/pkg/...` from `tests/*_test.go`
  (`go test ./tests/` from the instance):

  ```
  module github.com/acme/swarm
  go 1.26.2
  require github.com/camfinc/stormo v0.0.0
  replace github.com/camfinc/stormo => ./stormo
  ```

Worth having in every instance: a pinned-names test, one test per agent with scripts or unusual
config, and checks over every agent (cron delivery, approval timeouts).

## Vendoring the engine

The instance pins the engine; the engine never points at an instance. Pick one:

| | how | update | fits |
|---|---|---|---|
| **submodule** (recommended) | `git submodule add https://github.com/camfinc/stormo.git stormo`, then `cd stormo && go build -o bin/stormo ./cmd/stormo` | `git -C stormo pull && git add stormo` | editing engine and instance together |
| sibling checkout | build once, then `stormo --instance ~/acme …` or run it inside the instance | `git -C ~/stormo pull` and rebuild | one engine, several instances |
| release binary | `stormo` from a release (once published), pinned by version | install the new release | CI and servers that never edit the engine |

The agents' sidecars run the engine's own image, `stormo-sidecar:<engine version>`. A source
build makes it from `docker/sidecar.Dockerfile` the first time `stormo start` needs it (and on
every start from a checkout with uncommitted changes); a release pulls
`ghcr.io/camfinc/stormo-sidecar:<version>`. Locally the instance's files are mounted into it;
for ECS, `docker/instance.Dockerfile` adds them as one layer per agent.

## Starting a new instance

```sh
stormo new instance ~/acme --name "Acme Swarm"           # stormo.yaml, the group unit, .gitignore, git init
stormo new instance ~/demo --name "Demo" --template example   # the Acme example under the new name
```

The macOS app does the same from File › New Instance…, in `~/Library/Application Support/Stormo/Instances/`
by default. By hand, from a checkout of the engine:

```sh
cp -R stormo/examples/minimal ~/acme && cd ~/acme && git init
$EDITOR stormo.yaml units/ agents/        # your org, units and first agent
git submodule add https://github.com/camfinc/stormo.git stormo
(cd stormo && go build -o bin/stormo ./cmd/stormo)
stormo/bin/stormo check && stormo/bin/stormo secrets init   # then fill secrets.local.yaml (gitignored)
```

Add `dist/`, `.swarm/`, `workdir/` and `secrets.local.yaml` to the instance's `.gitignore`
before the first build.
