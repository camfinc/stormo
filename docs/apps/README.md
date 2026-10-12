# Stormo desktop apps

This is the shared feature ledger for the desktop apps. Update it in every PR that adds,
changes or removes desktop behavior; platform READMEs contain build instructions, not a second
feature backlog. Windows is the next delivery priority; Linux desktop packaging is deferred.

## Shared contract

- Engine behavior lives in Go. Read the versioned core API; perform mutations through the CLI
  with `--json`, an explicit instance, and `--target local`.
- Check API compatibility and instance identity before attaching or enabling actions.
- One active instance/core. Quitting the UI does not stop agents or the core.
- Never persist response bodies, credentials or process output in desktop settings.
- Private engine state uses POSIX permissions on Unix and current-user/SYSTEM ACLs on Windows.
- Setup must explain missing prerequisites in ordinary language, handle restart/retry, and
  never describe a dependency check or installer exit as verified runtime readiness.

## Feature parity

Status is based on source inspection, not a claim of runtime verification on every OS.
`Planned` means no implementation. `Initial` means a limited first implementation.

| Capability | macOS (`apps/macos`) | Windows (`apps/windows`) |
|---|---|---|
| Desktop shell | SwiftUI | Initial: Qt 6 / PySide6 |
| Runtime installation / repair | External local container runtime | Initial: native Windows engine + Docker Desktop checklist |
| New/open instance | Implemented | Initial: create default empty workspace only |
| Binary selection / API compatibility | Implemented | Initial: bundled stormo.exe / API and Windows path identity checks |
| Core start / stop / restart | Implemented | Initial: native detached core / graceful stop, ownership guard |
| Fleet / selected agent actions | Implemented | Initial: table, start / stop / restart / nap; selection tracked by agent ID |
| Native animated office | Implemented (SpriteKit) | Planned |
| Chat / conversations | Implemented | Planned |
| Providers / sign-in | Implemented | Planned |
| Agent creation / configuration / appearance | Implemented | Planned |
| Import / export | Implemented | Planned |
| Menu bar / tray | Implemented on macOS | Planned |
| Learning / review dedicated UI | Planned (CLI/web available) | Planned |
| Packaged installer | DMG build pipeline | Initial: per-user Inno Setup + CI |
| Signed release / clean-machine acceptance | Release credentials required | Planned |
| Remote/cloud instances | Deferred | Deferred |

## Review checklist

For each row changed: link the implementation and tests, record user-visible differences,
update the platform's known limitations, and distinguish source/test verification from a real
machine run. Platform-only capabilities may differ; commands and safety boundaries must not.

See [Windows architecture and acceptance](windows.md), [machine contracts](../api.md), and
[macOS build instructions](../../apps/macos/README.md).
