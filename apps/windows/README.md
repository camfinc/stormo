# Stormo for Windows (native preview)

A native WinUI 3 / C# desktop and native Go `stormo.exe`. Docker Desktop runs the Linux agents.
**No Stormo WSL distribution, Linux filesystem bundle or WSL command bridge.** Docker Desktop
may use WSL internally; that is Docker's implementation, not a second Stormo runtime.

Windows 11 x64 is the initial target. This is an unsigned development preview, not full macOS
parity. [Shared feature ledger](../../docs/apps/README.md) · [Architecture](../../docs/apps/windows.md).

## Install and use

1. Run `Stormo-Setup-native-x64-preview.exe`. .NET, Windows App SDK and the native engine are included.
2. Open Stormo. If needed, **Get Docker Desktop** opens the official installation guide.
   Install/open Docker Desktop and finish its setup, including any restart it requests.
   Accept Docker's terms yourself. No Linux distro/account or terminal is needed for Stormo.
3. Select **Check again**, then **Verify runtime**. Stormo checks Docker Desktop's Linux
   engine/Compose and runs a disposable `hello-world` container.
4. **Create workspace**, then **Start core and open Agents**. The initial workspace is empty.
5. The agent list provides Start, Stop, Restart and Nap now for existing agents. Closing the
   desktop leaves the native core and containers running. Core has explicit Start/Stop controls.

The workspace lives at `%LOCALAPPDATA%\Stormo\Instances\default`. App uninstall/upgrade leaves
it intact. Native core logs are inside that workspace at `.swarm\core\core.log`.

This slice does not yet provide agent creation, providers/sign-in, chat, or open/import of
another instance. It is a setup/operation foundation, not complete nontechnical onboarding.
Engineering evaluation can populate the workspace with the bundled CLI.

**Earlier WSL-preview testers:** any `Stormo` WSL distro and its data are left untouched.
This build does not use or silently migrate them. Preserve that data for an explicit migration.

## Development

Build on Windows 11 x64 with .NET 8 SDK, Visual Studio 2022's Windows application development
workload (Windows SDK / WinUI tooling), Python 3.13 for build scripts, and Go from go.mod.
Python is build tooling only; it is not shipped or required by users.

```powershell
dotnet run --project apps/windows/Stormo.Runtime.Tests -c Release
python apps/windows/installer/build-engine.py
python apps/windows/installer/smoke-engine.py
dotnet restore apps/windows/Stormo.Desktop -p:Platform=x64
python apps/windows/installer/collect-licenses.py
dotnet publish apps/windows/Stormo.Desktop -c Release -p:Platform=x64 -o apps/windows/dist/Stormo
./apps/windows/installer/smoke-desktop.ps1
& "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe" apps/windows/installer/stormo.iss
```

`build-engine.py` archives committed source and builds `assets/engine/bin/stormo.exe`; commit
engine changes before building. Tracked source is bundled for development sidecar builds.
The app is unpackaged and self-contained: neither .NET nor the Windows App Runtime needs a
separate installation. Inno Setup installs per user. Upgrading from the Qt preview removes
its obsolete `_internal` payload; user workspaces remain outside the installation directory.

The runtime contract checks cover ownership, API versions, local-only commands, argument
handling, stale agents and connection refusal versus an unresponsive/foreign service. Windows CI
also exercises the real detached core lifecycle and launches the published WinUI app. UI Automation
navigates its three sections and captures light/dark screenshots in the build artifact. These
checks do not prove clean-machine Docker installation, full agent operation, or visual accessibility
under every Windows display configuration. macOS cannot render or compile this WinUI shell.

## Windows icon

`assets/stormo.ico` comes from `docs/brand/mark.svg`, with 16, 24, 32, 48, 64, 128 and 256 px
frames. The .NET app host, WinUI window and Inno Setup use the same icon. Process and Start menu
shortcut share `CAMF.Stormo.Desktop`. CI verifies all icon frames in both final EXEs.

```sh
rsvg-convert -w 256 -h 256 docs/brand/mark.svg -o /tmp/stormo-icon.png
magick /tmp/stormo-icon.png -define icon:auto-resize=256,128,64,48,32,24,16 apps/windows/assets/stormo.ico
```

## Release gates

Clean-Windows/Docker Desktop verification of mounts, networking and agent startup; full
onboarding parity; backup/migration and upgrade/repair acceptance; dependency notices/compliance,
reproducible builds, Authenticode signing, SmartScreen behavior and release version wiring.

Deployment follows Microsoft's [self-contained Windows App SDK guidance](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/self-contained-deploy/deploy-self-contained-apps).
