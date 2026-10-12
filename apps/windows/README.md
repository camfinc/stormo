# Stormo for Windows (native preview)

A Windows Qt desktop and native Go `stormo.exe`. Docker Desktop runs the Linux agents.
**No Stormo WSL distribution, Linux filesystem bundle or WSL command bridge.** Docker Desktop
may use WSL internally; that is Docker's implementation, not a second Stormo runtime.

Windows 11 x64 is the initial target. This is an unsigned development preview, not full macOS
parity. [Shared feature ledger](../../docs/apps/README.md) · [Architecture](../../docs/apps/windows.md).

## Install and use

1. Run `Stormo-Setup-native-x64-preview.exe`. Python, Qt and the native engine are included.
2. Open Stormo. If needed, **Get Docker Desktop** opens the official installation guide.
   Install/open Docker Desktop and finish its setup, including any restart it requests.
   Accept Docker's terms yourself. No Linux distro/account or terminal is needed for Stormo.
3. Select **Check again**, then **Verify runtime**. Stormo checks Docker Desktop's Linux
   engine/Compose and runs a disposable `hello-world` container.
4. **Create workspace**, then **Start core and open Agents**. The initial workspace is empty.
5. The agent table provides Start, Stop, Restart and Nap now for existing agents. Closing the
   desktop leaves the native core and containers running. Core has explicit Start/Stop controls.

The workspace lives at `%LOCALAPPDATA%\Stormo\Instances\default`. App uninstall/upgrade leaves
it intact. Native core logs are inside that workspace at `.swarm\core\core.log`.

This slice does not yet provide agent creation, providers/sign-in, chat, or open/import of
another instance. It is a setup/operation foundation, not complete nontechnical onboarding.
Engineering evaluation can populate the workspace with the bundled CLI.

**Earlier WSL-preview testers:** any `Stormo` WSL distro and its data are left untouched.
This build does not use or silently migrate them. Preserve that data for an explicit migration.

## Development

Python 3.13 and Go as specified in the repository's go.mod:

```sh
python3.13 -m venv apps/windows/.venv
apps/windows/.venv/bin/python -m pip install -r apps/windows/requirements.txt
apps/windows/.venv/bin/python -m unittest discover -s apps/windows/tests -v
apps/windows/.venv/bin/python apps/windows/main.py
```

On Windows, use `.venv\Scripts\python.exe`. The shell can render on other systems, but the
runtime adapter is Windows-only. To build the bundled engine from a **committed checkout**:

```sh
python apps/windows/installer/build-engine.py
python apps/windows/installer/smoke-engine.py  # Windows only; no Docker required
```

`build-engine.py` archives tracked source, builds `assets/engine/bin/stormo.exe` from it and
keeps that source beside the binary for dev sidecar builds. No untracked secrets or user data
enters the bundle. Users need neither Go nor Git. The development sidecar's first build needs
internet access and Docker Desktop; release sidecars will be published with matching versions.

Windows CI runs native engine/Qt checks and the detached core lifecycle smoke test, then
PyInstaller and Inno Setup produce a per-user installer. The installer is not an admin service.
It doesn't install Docker silently or create/manage a WSL distribution.

## Windows icon

`assets/stormo.ico` is rasterized from the approved `docs/brand/mark.svg` with
16, 24, 32, 48, 64, 128 and 256 px frames. Qt, PyInstaller and Inno Setup use this
same icon. The process and Start menu shortcut share `CAMF.Stormo.Desktop` as their
Windows application identity. CI checks both final EXEs contain the exact icon frames.

To regenerate with librsvg and ImageMagick from the repository root:

```sh
rsvg-convert -w 256 -h 256 docs/brand/mark.svg -o /tmp/stormo-icon.png
magick /tmp/stormo-icon.png -define icon:auto-resize=256,128,64,48,32,24,16 apps/windows/assets/stormo.ico
```

## Release gates

Clean-Windows/Docker Desktop verification of mounts, networking and agent startup; full
onboarding parity; backup/migration and upgrade/repair behavior; dependency notices/compliance,
reproducible builds, Authenticode signing, SmartScreen behavior and release version wiring.
