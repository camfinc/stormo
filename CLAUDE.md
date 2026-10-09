# Stormo

The generic agent swarm engine, in Go (module `github.com/camfinc/stormo`). Read
[ARCHITECTURE.md](ARCHITECTURE.md) and [docs/instances.md](docs/instances.md) first.
`gofmt -l .` (empty), `go vet ./...` and `go test ./...` must pass before committing; the suite runs
on `examples/minimal` only.

- **Dependencies.** Few, pinned in go.mod/go.sum, std lib first. A new or upgraded module must be a
  release at least two weeks old (Go has no cooldown setting; check the date with
  `go list -m -json <module>@<version>`). Never `go get -u` wholesale.
- **No organisation in the engine.** Nothing under `pkg/` or `cmd/` names an org, a real agent, an
  AWS account or a domain. Org-specific values come from the instance's `stormo.yaml`
  (`pkg/instance`), passed explicitly (no globals); org-specific actions are the instance's
  `bridge/actions.yaml`. Tests use the example instance (Acme), never a real one.
- **Persisted formats are contracts**: the nap store layout and manifests (the local store lives in
  `agents/<id>/data/store`; its layout did not change), ledger lines, watermarks, baseline.json skill
  hashes, `.swarm/ports.json`, the core's auth files, the secrets files (`secrets.local.yaml` and each
  agent's `data/secrets.yaml`, read as its `agents.<id>` layer), agent.yaml's `format` (format 0 still
  loads; `stormo migrate agent` moves it to 1) and the `stormo export` zip (`stormo-agent/1`). Change
  one only with a migration; testdata/classify.json pins the snapshot rules.
- A new instance-level setting: add it to `stormo.yaml` (`pkg/instance` + docs/instances.md +
  examples/minimal), with a default derived from `slug` where one makes sense. Names that end up in
  deployed infrastructure or in agents' skills belong under `names:` and are documented as pinned.
- Secrets: manifests list env var NAMES only. Never write a token into a skill, script, cron prompt,
  test or example. Values live in the instance's `secrets.local.yaml` / Secrets Manager.
- `dist/` and `.swarm/` are generated, gitignored and belong to the instance; `.swarm/` can hold raw
  session snapshots (PII). So does `agents/<id>/data/` (naps, the agent's secret values, its env):
  it keeps its own `.gitignore`; never `git add -f` it, and export and import refuse a tracked one.
- **Agents are Stormo's standard** (docs/agent-standard.md): agent.yaml format 1 is engine-neutral;
  anything only one engine reads lives in `agents/<id>/engine/<kind>/` and goes through the
  `engine.Engine` interface (Runtime, Memory, schedules, FormatZero). Outside `pkg/engine/<kind>`
  nothing names an engine. A data export carries secret values in plain text by the owner's
  decision: keep its disclaimer, 0600 mode and the app's confirmation.
- Engine-specific files live under `agents/<id>/engine/<kind>/` (format 0: `agents/<id>/<engine>/`);
  engine-agnostic code goes through the `engine.Engine` interface (`pkg/engine`).
- Pin engine images (`engine.image_tag`); never deploy `latest`.
- `deploy render` only renders. Registering task definitions, creating services, buckets or secrets
  in AWS is a human step. Never run remote start/stop/restart or `secrets push --yes` on an
  instance without its owner's go.
- Shared documents: agents mount only `/shared/group` and `/shared/<their unit>`; never add a
  mount or access point that gives an agent another unit's directory.
- The learning ledger's PII scrub and promotion refusals (flagged or user-profile entries) are
  invariants; keep them.
- **The macOS app** (`apps/macos/`, SwiftUI, macOS 26+): no organisation in it beyond
  `Config/Identity.xcconfig`. It talks to the engine only through the core's HTTP API and
  `stormo --json` (docs/api.md, versioned by `version.API`); change either with the app in mind.
  Checks when it changed: `swift test --package-path apps/macos/StormoKit` and
  `xcodebuild -project apps/macos/Stormo.xcodeproj -scheme Stormo build`. Swift dependencies follow
  the same two-week rule.
