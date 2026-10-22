# What an agent container mounts

An agent sees three kinds of storage. Each has one owner and one mechanism, and none of them
depends on a particular engine: an engine only declares its `Layout` (home path, runtime uid,
snapshot rules) and gets the same three.

| storage | path in the container | mechanism | agent access | who else sees it |
|---|---|---|---|---|
| **Home**: working dir and config | `Layout.Home` (`$AGENT_HOME`) | plain volume, task-local; filled by `rehydrate` from the baseline plus the last nap | read-write | only that agent's sidecars |
| **Agent files**: naps, secret values, env | not mounted | core- and sidecar-mediated: `nap` writes `agents/<id>/data/store` (local) or `s3://…/<id>/` (AWS) | none; secrets arrive as env only | the owner |
| **Shared files** | `/shared/group`, `/shared/<unit>` | plain mount (local bind of `workdir/<layer>`, EFS access point on AWS); writes coordinated by the core's `fs_*` tools | read-write | every agent of the layer, and the core |

## Rules

- **Home.** Rebuilt on every start: the engine compiles the agent folder into a baseline, and
  `rehydrate` lays the last nap over it. The agent folder itself is never mounted. What the agent
  changes there is kept only through the nap, and reaches git only through the dream's review.
  It is never on shared storage (SQLite WAL), and exactly one task writes it.
- **Agent files.** The agent container never mounts `data/`. Locally the sidecars get a tmpfs over
  it and see only the store; secret values reach the agent as container env (`env_file`, or
  Secrets Manager on ECS).
- **Shared files.** Isolation is structural: a container mounts only `group` and its own unit, and
  on AWS its task role may mount only those two access points. Inside a layer everyone runs as
  the shared uid (`shared.PosixUID`), so the core adds what the filesystem does not have:
  attribution, locks, and confinement.
- **The core never follows a link out of a layer.** Agents can create symlinks on the mount, and
  the core runs on the host. Every `fs_*` operation goes through an `os.Root` opened on that one
  layer (`Workdir.open`), so a path that resolves to the host or to another unit's layer is
  refused, and `fs_write` replaces a link rather than writing through it. The scan raises a
  `shared_link_escape` finding for every link that leaves its layer.
- **Containers.** Every container of an agent (local) runs with `no-new-privileges` and without
  `NET_RAW`, `MKNOD`, `SYS_CHROOT`, `SETFCAP`, `AUDIT_WRITE` and `NET_BIND_SERVICE`. The user is
  left to the image, because engine images may boot as root and drop to their runtime user
  (Hermes: s6, then `hermes`; needletail lays the home out, runs as the runtime uid and starts
  what the agent executes as a second uid in the runtime's group, docs/needletail.md §7). A
  shared layer is reached by that second uid too: through an EFS access point every operation
  is the access point's POSIX user (`shared.PosixUID`) whoever makes it, and a local layer must
  be group-writable for the shared uid's group (2775, as the access points create them).

## Next steps (not built)

1. **Split home into config (read-only) and state (read-write).** The compiled baseline (SOUL,
   engine config, managed skills, scripts) would be read-only, and naps would cover only state.
   Changes to config would arrive as proposals. This needs an engine that tolerates a read-only
   config. Hermes chowns and may rewrite `config.yaml` at boot, so for Hermes this stays a
   read-write home.
2. **Shared read-only, with writes only through the core** (`shared.writes: core`). This makes
   locks and ownership enforced rather than advisory. It needs:
   - a core write path for binaries and large files (upload or put), since `fs_write` takes up to
     1 MiB of text;
   - `shared_docs.py` writing through the core;
   - a core that serves `fs_*` on AWS too (today it is local only).
3. **Engine-neutral runtime uid.** `shared.PosixUID` (10000) is Hermes's user today. It should be
   an instance setting that every engine's `Layout` and the EFS access points follow.
