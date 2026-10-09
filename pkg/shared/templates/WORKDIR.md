
## Coordinating with other agents (local runs on the swarm core)

Here the shared space is watched by the swarm core: it records who creates and changes each file,
and agents take locks so they do not overwrite each other. Use the core's tools
(`mcp_core_fs_*`) or, from the terminal, the helper:
`python3 ${SKILL_DIR}/scripts/workdir.py <command>`.

- **Before changing a file another agent may be working on**, check it: `fs_info` (or
  `workdir.py info <path>`) names its last writer and any locks.
- **Lock what you are about to edit**: `fs_lock` with `kind`
  - `hard`: exclusive, you are writing it (30 min, renew with `fs_renew`). Others' `fs_write` is
    refused and any other write to it is reported to the owner.
  - `soft`: advisory, you are working on it, coordinate first (2 h).
  - `temp`: a 5-minute lease for one quick operation; never renewed.
  Release with `fs_unlock` as soon as you are done. Locks of a stopped agent are released.
- If a path is locked by someone else, wait or ask them with `msg_send`; never work around a lock.
- **Write through the core when you can**: `fs_write` records the change as yours exactly. After
  changing a file any other way (terminal, script), run `fs_note` (`workdir.py note <path>`), or
  the change may be recorded as `unknown`.
- `fs_ls` / `workdir.py ls <dir>` lists a directory with last writers and locks.
