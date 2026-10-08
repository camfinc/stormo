<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/logo-dark.svg">
    <img src="docs/brand/logo-light.svg" alt="Stormo" width="340">
  </picture>
</p>

**Run a team of AI agents like a small company.** Stormo takes your organisation's agents, defined
as plain files in git, runs them on your laptop or on AWS, watches them work, and turns what they
learn on the job into reviewed changes you can merge.

![The office: every agent at its desk, live](docs/office.png)

*The office, served by `stormo core`: each unit is a room, each agent sits at a desk dressed for its
role, and the name tags say what it is doing right now (a Slack turn, a scheduled job, idle). The
little robot carries each agent's latest snapshot back to the server room and notes what changed.
This one is the bundled demo (`go run ./examples/office-demo`), a made-up company with simulated
activity.*

## What it does

- **Agents as files.** An *instance* is one organisation's directory: `stormo.yaml`, units, agents
  (manifest, personality, skills, schedules), personas and bridge actions. It versions, reviews and
  moves like any other repo ([docs/instances.md](docs/instances.md)).
- **One place at a time, anywhere.** The same agent runs in a local Docker Compose project or as
  an ECS task, and `stormo handoff` moves it between the two with its memory, skills and history
  (never its credentials).
- **Learning you can review.** Sidecars snapshot what each agent learns ("naps"). A dream folds
  the naps into a ledger of proposed lessons and skill changes, scrubbed of personal data, for a
  person to accept, reject or promote to the whole unit. Accepted lessons ship with the next build.
- **Isolation by design.** Agents see only their unit's lessons, documents and bridge actions, plus
  what is shared group-wide. Secrets are names in git and values in one local file or AWS Secrets
  Manager.
- **A control plane with a face.** `stormo core` serves the office above, one ChatGPT-plan model
  gateway for every local agent, and the fleet's status.
- **Pluggable engine.** Agents run on [Hermes](https://hermes-agent.nousresearch.com/docs/) today,
  behind an interface built for more.

Stormo is a single Go binary: the CLI you run, the core service, and the sidecar inside every
agent's task.

## Quick start

```sh
git clone https://github.com/camfinc/stormo.git && cd stormo
go build -o bin/stormo ./cmd/stormo          # Go 1.26
ln -s "$PWD/bin/stormo" ~/.local/bin/stormo  # anywhere on your PATH

go run ./examples/office-demo                # the office above, at http://127.0.0.1:18700/
```

Then make it yours. Copy the example instance, describe your organisation and its first agent,
and start it:

```sh
cp -R examples/minimal ~/acme && cd ~/acme && git init
$EDITOR stormo.yaml units/ agents/
stormo check                # validate and compile every agent
stormo secrets init         # secrets.local.yaml (gitignored, 0600): fill in the values
stormo start                # build, then run every agent locally (Docker or OrbStack)
stormo core up              # the office and the model gateway on http://127.0.0.1:18600/
```

Stormo finds the instance from the current directory (or `--instance <dir>`, or
`STORMO_INSTANCE`). Vendoring the engine inside the instance as a git submodule keeps both in step
([docs/instances.md](docs/instances.md)).

## How it fits together

```
 instance (git)                    stormo                          where agents run
 ──────────────                    ──────                          ────────────────
 stormo.yaml, units/, agents/ ──▶ build ─▶ baseline ───────────▶ local compose │ ECS task
                                                                  rehydrate ─▶ agent ◀─ nap
 learnings/ledger.jsonl  ◀── review ◀── dream ◀── naps (dir or S3) ◀────────────────┘
```

- **Build** compiles an agent for its engine: personality, skills, schedules, config, and the
  lessons a reviewer accepted.
- **Rehydrate** starts every task from that baseline plus its latest nap; **nap** keeps snapshotting
  while it runs and once more on shutdown.
- **Dream** reads only what an agent learned (never its conversations) and proposes it back.

The design in depth: [ARCHITECTURE.md](ARCHITECTURE.md). The core and the office:
[docs/core.md](docs/core.md).

## Everyday commands

| | |
|---|---|
| `stormo` | status of every agent: state, last nap, lessons waiting for review |
| `stormo start` · `stop` · `restart [agent…]` | local by default; `-r` targets ECS (remote changes ask first) |
| `stormo handoff <agent> --to remote` | stop here, carry the latest nap over, start there |
| `stormo logs <agent> -f` · `chat <agent> "hello"` | follow an agent, or talk to its local API |
| `stormo learn [agent…]` | nap now, fold naps into proposals, show what to review |
| `stormo learn list` · `accept` · `reject` · `promote` | review lessons; `learn skills` for skill proposals |
| `stormo inspect <agent>` | everything the engine computes for an agent, as JSON |
| `stormo bench <agent>` | scenario checks, mocked or against the real engine image |
| `stormo review` | a read-only health sweep of the local fleet |
| `stormo core up` · `status` · `login` | the office, the model gateway, the ChatGPT sign-in |

`stormo --help` lists everything; `swarm` works as a second name for the binary.

## Shipping to AWS

```sh
stormo secrets push [agent…]         # key-name diff against Secrets Manager; --yes writes
stormo deploy render-shared          # one-time EFS setup for the shared document space
stormo deploy render <agent>         # task definition + task role policy, and the aws commands
stormo build <agent>                 # then one sidecar image per agent, from the instance:
docker build -f <engine>/docker/instance.Dockerfile --build-arg STORMO_IMAGE=stormo-sidecar:<version> \
  --build-arg AGENT=<agent> --platform linux/arm64 -t <registry>/<names.resource>-<agent>:<tag> .
```

Rendering never calls AWS. Registering task definitions and creating services, buckets or secrets
stays a human step.

## Repository

```
cmd/stormo/          the CLI
pkg/                 the engine (instance, manifests, build, learning loop, core, deploy, …)
docker/              the sidecar image and the per-agent ECS layer
examples/minimal/    a complete example instance: the template and the test fixture
examples/office-demo the office with simulated activity (the screenshot above)
docs/                instances, the core and the office; docs/brand: the logo
tools/brandgen/      generates the logo files
```

## Develop

```sh
gofmt -l . && go vet ./... && go test ./...
go build -o bin/stormo ./cmd/stormo && bin/stormo --instance examples/minimal check
```

Contributor notes, including how dependencies are pinned: [CLAUDE.md](CLAUDE.md).
