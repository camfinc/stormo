# Stormo for Windows

Status: first implementation in progress. Target: Windows 11 x64. Linux packaging follows later.

## Architecture decision

Use Qt 6 through its official PySide6 bindings for the desktop. It supports Windows and Linux,
provides accessible native desktop controls without a web server/WebView, and lets the later
Linux port reuse the UI and protocol adapter. Python and Qt ship inside the installer: users
must not install Python, Go, Git, or run a terminal. Keep UI and process/runtime logic separate.
GTK is a better fit for a Linux-first GNOME app; WinUI would require a separate Linux UI.

The engine runs in WSL 2. `pkg/core/cli.go` uses Unix session/process signals, and the agents
need Linux containers. Do not claim a native Windows engine or just cross-compile the CLI.
Keep instance data in the Linux filesystem; use Windows localhost forwarding for API reads.

Owner decision: guided Docker Desktop setup initially. Bundle a dedicated `Stormo` WSL 2
filesystem with the Linux CLI, then guide users to enable Docker Desktop integration for it.
No separately installed Ubuntu/user-account setup and no private Docker daemon. The desktop
opens Docker's official installation guide; users retain control of its license acceptance.
Elevation is requested only for WSL installation. Restart and retry re-detect actual state.

Implemented first slice: prerequisite checks, archive integrity/ownership guards, import,
container execution probe, default empty instance, core start/stop and fleet actions.
See [the Windows README](../../apps/windows/README.md) for build commands and current limits.

## Delivery gates

1. Testable runtime detection, command adapter, installer skeleton and shared feature ledger.
2. First-run UI, guided prerequisite setup, create instance, core lifecycle and fleet actions.
3. Windows CI produces a per-user setup EXE including Python/Qt and the matching engine.
4. Clean Windows VM acceptance: absent WSL, restart, cancelled UAC, no virtualization,
   no network, already-installed runtime, wrong core, interrupted setup, repair and upgrade.
5. Signed binaries/installer and published runtime artifacts before calling this a release.

Uninstall must preserve instances and agent data. Deleting them is a separate explicit action.
Do not silently accept third-party license terms, modify someone else's WSL distribution,
expose Docker TCP, or bind the core on all interfaces.

## Sources for the platform decision

- [Qt for Python deployment](https://doc.qt.io/qtforpython-6/deployment/index.html)
- [Microsoft WSL installation](https://learn.microsoft.com/en-us/windows/wsl/install)
- [Windows-to-WSL localhost networking](https://learn.microsoft.com/en-us/windows/wsl/networking)
- [Docker Desktop terms](https://docs.docker.com/subscription-billing/desktop-license/)
- [Inno Setup per-user installation](https://jrsoftware.org/ishelp/topic_setup_privilegesrequired.htm)
