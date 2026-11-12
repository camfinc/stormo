# Remote cores and distributed agents

Status: **built**, all five phases (§8). This page was written as the plan, so "today" below means
before it. What shipped differs where docs/core.md notes it: sync is per runner host (not per
sidecar), agents link to a core when they are on it (model.local via core), ECS agents keep EFS,
and a host other than the core's runs agents as a `runner` placement (`stormo runner`). Owner
decisions of 2026-10-17:

1. A client (app, CLI) connects to a core by **URL + token**: local or remote, same protocol.
2. **Every agent depends on a core**, local or on AWS, and **survives a temporary core outage**.
3. **Cores run locally or remotely.**
4. **Agents can be anywhere**: on the core's host, in a cluster, or in separate networks.
5. **The ChatGPT plan is served by whichever core holds the sign-in, to the agents on it.**
   (Revised 2026-11-10; it read "the ChatGPT subscription stays local".) A core on another machine
   signs in there through a forwarded callback port (docs/core.md §1). The engine does not judge
   compliance with OpenAI's terms: every sign-in shows the notice and links them. Agents on ECS, or
   reaching a core only over the link from other networks, keep their API connections: routing them
   through a core's plan gateway is a separate feature, not built.

Today's core assumes the opposite on almost every point: loopback only, a few open routes,
agents reached on `127.0.0.1:<port>`, lifecycle through `docker compose` on the same host, shared
space as bind mounts, and agents on ECS without any core. This plan changes those assumptions in
phases. Each phase can be shipped on its own.

## 1. The model

```
          clients (app, CLI)                  URL + token, TLS
                 │
          ┌──────▼──────┐
          │    core     │  control plane: registry, bus, shared space, capabilities, learning,
          │ local|remote│  findings, ChatGPT gateway
          └──▲───────▲──┘
   outbound  │       │  outbound (agents and runners dial the core; the core dials no one)
   ┌─────────┴─┐   ┌─┴───────────┐
   │  runner   │   │   runner    │  starts/stops agents where they live: local docker,
   │ (docker)  │   │ (ECS / k8s) │  ECS API, later k8s
   └─────┬─────┘   └─────┬───────┘
      agents           agents      each with a link to the core
```

- **Core**: one per instance (unchanged). It never needs to reach agents or runners: they
  connect to it. That is what makes separate networks work: only the core needs to be reachable.
- **Agent link**: every agent keeps one outbound, authenticated, long-lived connection to its
  core (WebSocket over TLS; HTTP long-poll as the fallback). It carries heartbeats and status
  (replacing the core's 3 s polling of `127.0.0.1:<port>`) and core→agent commands (wake runs,
  nap requests, config-changed notices). The agent→core calls stay plain HTTPS: gateway, ingest,
  MCP.
- **Runner**: the part that creates containers. `local` is today's compose code moved behind an
  interface. `ecs` is today's `-r` path (AWS API with a role) moved behind the same interface. A
  runner is embedded in a local core, or runs separately and connects to a remote core the same
  way an agent does. The core asks runners to act; it never shells out to someone else's Docker.
- **Client**: the app and the CLI do everything through the core's API, so they need no shell on
  the core's host. The local CLI keeps working against a local core.

## 1a. Transport: one live wire plus plain HTTPS

- **The link** is one long-lived **WebSocket over TLS** (`wss://<core>/link`), opened by each
  agent's sidecar, runner and app. It starts as an ordinary HTTPS request, so it works through
  load balancers, proxies, Tailscale and SSH tunnels. The key or token goes in the handshake.
- On it, each message is a JSON envelope `{id, seq, ack, type, body}`: **events** (activity,
  status, heartbeats), **commands** (wake, nap, config changed, cancel), **requests with
  replies** in either direction (matched by `id`), and **streams** (progress and partial output
  of long jobs).
- A ping every ~15 s detects a dead connection and keeps idle-closing load balancers away (an
  ALB closes after 60 s by default). Reconnects back off with jitter. Each side resumes from the
  other's last acknowledged `seq`, so events in the spool are sent again once, then the two sides
  reconcile state. Queues are bounded on both sides, and a slow consumer never blocks the agent.
- **Plain HTTPS stays** for model calls (the OpenAI-compatible gateway, streamed), MCP (its own
  HTTP transport) and bulk files (shared space, capability inputs and outputs, through short-lived
  URLs handed out on the link). Control goes on the wire; bulk data goes over HTTPS.
- The app subscribes to fleet changes on the wire instead of polling `/api/fleet`.
- Libraries: Swift's `URLSessionWebSocketTask`. Go: one small WebSocket module, pinned (the
  two-week rule).

## 2. Addresses: each agent knows its core

- `stormo.yaml` gets `core.url` (what agents and clients dial), e.g.
  `https://core.example.internal:18600`. Unset means a local core
  (`host.docker.internal:<instance port>`, today's behaviour).
- One core can need several addresses: containers on the same host reach it differently from
  ECS tasks. Placements get their own: `core.urls: {local: …, aws: …}`. The build writes the
  matching one into each agent (`manifest.CoreAt` grows from a port to a URL).
- The core's identity is the instance claim id plus a TLS certificate. The app pins the
  certificate the first time it connects (trust on first use) unless it chains to a CA you trust.

## 3. Authentication: on every route

| Principal | Credential | Can |
|---|---|---|
| owner / operator | **personal tokens**: named, scoped (`read`, `operate`, `admin`), with expiry, revocable, stored hashed in core.db; `stormo core token add/list/revoke` | the API by scope |
| agent | **agent key** (`SWARM_CORE_KEY`, per agent, rotatable) | gateway (local core only), ingest, MCP, its link, its own data |
| runner | **runner token** (per runner, scoped to placements) | report agents, receive lifecycle orders |

- **Nothing is anonymous** except `/health`. `/api/fleet`, `/api/core`, `/api/instance`,
  timelines and avatars move behind `read`. A local core with no `core.url` keeps today's
  loopback trust, as an option that is on by default.
- **TLS by default** whenever the core listens beyond loopback: with its own certificate files, or
  behind a proxy that terminates TLS (`core.trusted_proxy`). `core.tls: off` is allowed only on a
  private address (an SSH tunnel or a VPN carries the encryption) and shows as a warning.
  Tokens are required either way.
- Today's `owner.token` becomes the first `admin` token. The app stores tokens in the Keychain,
  one per connection.
- **Rule change:** agent keys can no longer stay local-only, because ECS agents now need one.
  They move into each agent's normal secrets (Secrets Manager `<names.secret>/<agent>`), are
  generated by the core, and rotate with a grace period so old and new both work while agents
  pick up the new key.
- Every action is audited with its token's name: decisions, restarts, secrets, deploys.

## 4. Surviving a core outage

An agent has to keep working its own turns while the core is down, and catch up afterwards.

| Function | While the core is down |
|---|---|
| models | local core: the existing fallback to the agent's own provider; remote: agents call API providers directly, so model calls never depend on the core |
| ingest (activity, hooks) | written to a bounded spool on the agent (size and age caps; oldest dropped first) and sent again when the link returns. Events carry ids, so a resend never counts twice |
| bus (messages) | `msg_send` goes to a local outbox and is delivered later, keeping order per thread. Reading the inbox shows what was last synced, marked as stale |
| shared files | see §5. Locks have expiry times, so the core's view stays consistent when it comes back |
| capabilities | jobs already started keep running in their worker. New calls fail fast with "core unreachable, retry later" |
| commands (wake, nap) | missed while the link is down. When it comes back, the core gets the agent's state from it and re-sends anything outstanding |
| learning (nap/dream) | naps already go to the agent's store; the dream runs once the core is back |

- The agent's sidecar holds the spool, the outbox and the link. Hermes doesn't change: it talks
  to `localhost`, and the sidecar forwards to the core or buffers.
- The app shows each agent's link state (`connected`, `buffering for 4 min`, `lost`). An agent
  without a link for longer than a threshold becomes a finding.

## 5. Shared space across machines

Bind mounts only work when agents and the core share a host. Planned replacement:

- **Source of truth**: the core's file API (the `fs_*` tools exist already) on top of storage
  chosen per instance: a local directory (today), S3, or EFS.
- **Agents** keep a local working copy under `/shared/<layer>`, synced by the sidecar. Writes go
  through the core with the existing checks for locks and attribution. Outside a lock, conflicting
  writes are kept as `.conflict` copies; they are never merged silently.
- The rule that agents mount only group and their own unit becomes **sync** only group and
  their own unit. The core enforces it from the agent's key.

## 6. Capabilities (heavy calls)

As discussed: the instance declares capabilities and agents get grants in their manifests. The
core acts as a broker: it checks permissions, queues, meters, scrubs and starts jobs, while the
work runs in worker containers placed by a runner. With a remote core and agents everywhere, a
capability's workers can run on whichever runner has the hardware (e.g. a GPU host), and every
agent reaches them through its core. Pilot: media renders.

## 6a. Deployment adapters

Where a core, a runner or an agent runs, and where its secrets and shared files live, is decided
by a **deployment adapter**, not by code paths written for one platform. Adapters are a Go
interface in the engine; a new one (k8s, Fly, Nomad…) is added without touching the others.

| Adapter | Runs things with | Secrets | Shared files (default) | Reaching the core |
|---|---|---|---|---|
| `local` | docker compose on this host (today) | `secrets.local.yaml` + per-agent `data/secrets.yaml` (today) | a local directory (bind mounts, today) | loopback / `host.docker.internal` |
| `vps` | docker compose on a host over SSH (a runner on the host dials the core) | a 0600 secrets file on the host (encryption at rest optional later) | a local directory on the host | whatever the owner chose: public URL, SSH tunnel, VPN |
| `ecs` | the AWS API with a role (today's `-r` path) | Secrets Manager `<prefix>/<agent>` (today) | **EFS** with access points per layer (already deployed); S3 as an option | the core's URL for the VPC |

- Every adapter implements the same operations: place/start/stop/restart/status for agents,
  put/check secrets (values never come back), provision shared storage, and render what it needs.
  Rendering stays separate from applying. Applying to production keeps its confirmation step (§3).
- **Agent keys go where the adapter keeps secrets**: Secrets Manager on `ecs`, the secrets file on
  `local` and `vps`. That makes the old local-only rule a rule of the `local` adapter instead of a
  global one.
- **Shared files support both S3 and EFS**, each adapter with its own default. When the core runs
  outside the VPC (e.g. on a VPS) it can't mount EFS. The `ecs` runner then serves the core's
  file operations from inside the VPC, over the link.
- The core itself is placed by an adapter too: on this Mac (`local`), on a VPS (`vps`), or as an
  ECS service (`ecs`). Its state (core.db, instance checkout, token store) goes on persistent
  storage; backing it up is the owner's, not the engine's.

## 7. The app and the CLI

- **Connections**: "This Mac" (local core, found through the run files) or "Remote" (URL + token,
  certificate pinned). An instance is identified by its claim id and name, not by a file path.
- Every action goes through the core's API: start/stop/restart (sent to a runner), config edits
  (the core writes the instance files), secrets (names and whether set, never values back),
  review, chat. The local CLI path stays for local instances and offline work.
- **Remote instances' files**: a remote core owns a checkout of the instance. Edits made
  through the API are committed there as the token's owner. You decide when to push. The core
  never pushes on its own.

## 8. Phases

1. **Auth everywhere**: personal tokens with scopes, every route protected, an audit log; then TLS
   and listening beyond loopback (`core.listen`, `core.url`). The app's "Remote" connection is
   read-only at first. *Outcome: you can watch a remote core with a URL and a token.*
2. **Agent link and buffering**: the sidecar's link, spool and outbox. Status from heartbeats; wake
   runs over the link. Agent keys move into Secrets Manager. *Outcome: ECS agents report to a
   core and survive outages.*
3. **Runners**: `local` and `ecs` behind one interface; lifecycle through the core's API.
   *Outcome: the app starts and stops agents on any placement through any core.*
4. **Shared space across machines**: storage backends and sidecar sync. *Outcome: agents in
   separate networks share documents.*
5. **Capabilities**: the broker, jobs and workers, starting with the media-render pilot.

## Decisions (owner, 2026-10-17)

1. **How the core is reached:** by any means, and the core assumes none. Public URL with TLS,
   VPN, or an SSH tunnel all work. Behind an SSH tunnel or a VPN, a core can listen on a private
   interface with TLS off (`core.tls: off`, allowed only on a non-public address and flagged in
   `stormo core status`); **tokens are required regardless**. Setting up the tunnel is the owner's
   infrastructure (see Out of scope).
2. **Where a remote core runs:** a **VPS** or **ECS**, both through deployment adapters (§6a).
3. **Agent keys and secrets:** follow the deployment adapter. Secrets Manager on `ecs`, the
   adapter's own store elsewhere. Adapters are pluggable so new targets can be added.
4. **Production lifecycle from the app:** yes, an `operate` token plus a confirmation for each
   action. Talking to agents and approving their work doesn't change in any environment: it stays in
   Slack (and the app's Chat) for local and remote fleets alike.
5. **Shared-space storage:** S3 and EFS both supported. The `ecs` adapter defaults to the easier
   one, which is **EFS**: it is already deployed with access points per layer, and ECS agents keep a
   plain mount.

## Out of scope (owner, 2026-10-17)

- **Networking.** Agents, runners and clients are given a reachable `core.url` and assume it
  works. VPNs, tunnels, jump boxes and firewall rules are infrastructure the owner provides; the
  engine neither builds nor manages them.
- **Backups** of the core's state (core.db, the instance checkout, the token store). The engine
  documents what to back up and nothing more.
