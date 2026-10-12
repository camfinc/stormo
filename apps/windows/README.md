# Stormo for Windows (preview)

Qt 6 / PySide6 desktop, with the existing Go engine in a dedicated `Stormo` WSL 2 distribution.
Windows 11 x64 is the initial target. **This is an unsigned development preview, not a release
or full macOS equivalent.** Feature parity lives in [docs/apps](../../docs/apps/README.md).

## User installation flow

1. Run the per-user `Stormo-Setup-x64-preview.exe`. It includes Python, Qt and the Linux runtime;
   none of those needs a separate user installation.
2. Open Stormo. **Prepare Windows** launches Microsoft's WSL installer with a Windows elevation
   prompt. If Windows requests a restart, restart and reopen Stormo. **Check again** resumes by
   detecting what is actually installed; there is no automatic reboot.
3. **Prepare Stormo** imports the bundled, checksum-verified WSL filesystem. It never overwrites
   an existing distro, installs into another distro, or changes the default distribution.
4. **Get Docker Desktop** opens Docker's official installation guide. Install/open Docker Desktop,
   accept its terms yourself, then enable **Stormo** in **Settings → Resources → WSL Integration**
   and choose **Apply & restart**. The initial preview does not automate Docker's installer/settings.
5. **Check again**, then **Verify runtime**. Stormo requires Linux Docker + Compose and runs a
   disposable `hello-world` container. **Create workspace** creates an empty local instance.
6. In **Core**, start the core. **Agents** reads the fleet and provides selected-agent Start,
   Stop, Restart and Nap now actions. Closing Stormo leaves the engine/agents running.

An empty workspace is intentional: this slice does not yet include agent creation, provider
sign-in or importing an existing workspace. It is a setup and operation foundation, not yet a
nontechnical user's complete agent onboarding flow. The CLI can populate this workspace during
engineering evaluation. No secret or model configuration is fabricated by setup.

Instances live in `/home/stormo/Instances/default` inside the `Stormo` distro. Its disk is under
`%LOCALAPPDATA%\Stormo\Runtime`. App uninstall preserves that disk and the WSL registration.
Never delete/unregister it as a repair step: it holds user data. Runtime updates/migrations and
backup/restore are release gates; an app upgrade currently does not update the imported engine.

## Development

Python 3.13 (matching CI):

```sh
python3.13 -m venv apps/windows/.venv
apps/windows/.venv/bin/python -m pip install -r apps/windows/requirements.txt
apps/windows/.venv/bin/python -m unittest discover -s apps/windows/tests -v
apps/windows/.venv/bin/python apps/windows/main.py
```

On Windows use `.venv\Scripts\python.exe` in place of `.venv/bin/python`. On other platforms,
Qt can render/test the shell, but production runtime actions are Windows-only.

Build the runtime on Linux with Go + Docker, from a clean committed checkout:

```sh
apps/windows/installer/build-runtime.sh
```

This exports an Ubuntu filesystem with an unprivileged `stormo` account, a marker, the engine
and tracked source for development sidecar builds. It contains **no Docker daemon**; Docker
Desktop supplies the integration. No npm/Python credentials, instance data, local git worktree
metadata or untracked files enter the archive. It uses a fixed WSL distro name and fixed Linux
instance path; CLI values remain argument-array elements, never interpolated shell text.

The `windows-app` GitHub Actions workflow builds that bundle, tests/freezes the Qt app on
Windows, then uses Inno Setup to produce the setup EXE as an artifact. It does not publish a
release or mutate a developer's WSL installation. CI packaging is not clean-machine acceptance.

## Release work still required

- Clean Windows acceptance for WSL activation/reboot, Docker integration, localhost reachability,
  agent networking, initial image builds, cancelled elevation, offline retry and corrupted bundles.
- Agent creation, providers, sign-in, chat, multiple/open/imported instances and native office.
- Bundle/build identity checks across app/runtime upgrades, data migration and repair UX.
- Reproducible dependency locks/SBOM, Qt LGPL notices and relinking/source compliance review,
  Ubuntu package notices, Authenticode signing and SmartScreen/reputation testing.
- Tag/version wiring and a matching published sidecar before public release. Preview's `dev`
  engine builds the sidecar from its bundled source; first agent start requires internet access.
