# Stormo

An agent swarm engine. Stormo takes an **instance** (one organisation's agents, units, personas and
bridge actions: [docs/instances.md](docs/instances.md)), compiles each agent for an engine (Hermes
today, swappable), runs it locally or on AWS ECS, benches it, and folds what the running agents
learn back into git through a reviewed nap/dream loop. Design: [ARCHITECTURE.md](ARCHITECTURE.md).

Stormo is one Go binary: the CLI you run, the core service, and the sidecars inside every agent's
task (`stormo rehydrate`, `stormo nap --loop`, from the `stormo-sidecar` image).

## Install

```sh
go build -o bin/stormo ./cmd/stormo         # Go 1.26
ln -s "$PWD/bin/stormo" ~/.local/bin/stormo # anywhere on PATH; `swarm` works as a second name too
cd <your instance>                          # or --instance <dir> / STORMO_INSTANCE=<dir>
stormo secrets init                         # creates secrets.local.yaml (gitignored, 0600) in the instance
```

A binary built inside an instance's `stormo/` checkout finds that instance from any directory.
No instance yet? Copy [examples/minimal](examples/minimal) (docs/instances.md, "Starting a new
instance").

Fill `secrets.local.yaml`: shared keys under `shared:`, unit keys under `units.<unit>`, agent keys
under `agents.<id>`, local-only values under `local:` in the same shape. Then `stormo secrets check`.

## Operate agents: one command, local by default

```sh
stormo                        # status of every agent: state, last nap, lessons waiting for review
stormo start [agent…]         # build + start containers (OrbStack / Docker); builds the sidecar image if needed
stormo stop  [agent…]         # stop; the final nap is kept in .swarm/store
stormo restart [agent…]       # recycle like a redeploy: final nap → fresh home → rehydrate
stormo handoff <agent> --to remote   # stop here (final nap) → carry its naps → start there
stormo learn [agent…]         # nap now, fold naps into proposals, show what to review
stormo logs <agent> -f · stormo chat <agent> "hello" · stormo nap-now <agent>
stormo inspect <agent>        # everything the engine computes for the agent, as JSON

stormo -r                     # the same against ECS (--remote, or SWARM_TARGET=remote)
```

Locally an agent can run on a ChatGPT subscription instead of its API key (`engine.local: {via:
core}`): `stormo core up` holds the one login and serves the model to every local agent; it also
serves the office UI at <http://127.0.0.1:18600/>.

Review what the agents learned, then ship it:

```sh
stormo learn list <agent> --status proposed
stormo learn accept|reject <agent> <id…>  # also: edit --text, pin, promote --scope unit|group
stormo learn skills <agent>               # skill-accept | skill-reject <dir>
stormo bench <agent> --runner docker      # regression gate before committing accepted changes
```

## Ship to AWS

```sh
stormo secrets push [agent…]              # diff key names vs Secrets Manager; --yes writes
stormo deploy render-shared               # one-time EFS setup for the shared document space
stormo deploy render <agent>              # dist/<agent>/taskdef.json + task-policy.json, prints aws commands
stormo build <agent>                      # then, from the instance, one sidecar image per agent:
docker build -f <engine>/docker/instance.Dockerfile --build-arg STORMO_IMAGE=stormo-sidecar:<version> \
  --build-arg AGENT=<agent> --platform linux/arm64 -t <registry>/<names.resource>-<agent>:<tag> .
```

`stormo version` names the engine version; `stormo start` builds `stormo-sidecar:<version>` from
`docker/sidecar.Dockerfile` the first time it is needed. Rendering never calls AWS; registering
task definitions and creating services, buckets or secrets is a human step.

## Develop

```sh
gofmt -l . && go vet ./... && go test ./...     # the suite runs on examples/minimal only
go build -o bin/stormo ./cmd/stormo && bin/stormo --instance examples/minimal check
```
