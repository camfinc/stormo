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
- Setup must explain missing prerequisites in ordinary language, handle restart/retry, and
  never describe a dependency check or installer exit as verified runtime readiness.

## Feature parity

Status is based on source inspection, not a claim of runtime verification on every OS.
`Planned` means no implementation. `Initial` means a limited first implementation.

| Capability | macOS (`apps/macos`) | Windows (`apps/windows`) |
|---|---|---|
| Desktop shell | SwiftUI | Planned: Qt 6 / PySide6 |
| Runtime installation / repair | External local container runtime | In progress: guided first run |
| New/open instance | Implemented | Planned |
| Binary selection / API compatibility | Implemented | Planned |
| Core start / stop / restart | Implemented | Planned |
| Fleet / selected agent actions | Implemented | Planned |
| Native animated office | Implemented (SpriteKit) | Planned |
| Chat / conversations | Implemented | Planned |
| Providers / sign-in | Implemented | Planned |
| Agent creation / configuration / appearance | Implemented | Planned |
| Import / export | Implemented | Planned |
| Menu bar / tray | Implemented on macOS | Planned |
| Learning / review dedicated UI | Planned (CLI/web available) | Planned |
| Packaged installer | DMG build pipeline | Planned |
| Signed release / clean-machine acceptance | Release credentials required | Planned |
| Remote/cloud instances | Deferred | Deferred |

## Review checklist

For each row changed: link the implementation and tests, record user-visible differences,
update the platform's known limitations, and distinguish source/test verification from a real
machine run. Platform-only capabilities may differ; commands and safety boundaries must not.

See [Windows architecture and acceptance](windows.md), [machine contracts](../api.md), and
[macOS build instructions](../../apps/macos/README.md).
