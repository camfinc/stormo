# Stormo for Windows

Status: native Windows preview. Target: Windows 11 x64. Linux desktop packaging follows later.

## Architecture

The WinUI 3 desktop launches the bundled **Windows `stormo.exe` directly**. The Go core,
instance files, configuration, logs and database all live on Windows. Docker Desktop runs the
Linux agent and sidecar containers. Stormo neither installs nor invokes WSL, imports Linux
filesystems, manages a Linux user, nor maintains its own Docker daemon.

Docker Desktop may use WSL 2 or Hyper-V internally; Docker owns that setup and lifecycle.
The owner selected guided Docker Desktop setup initially. The app links Docker's official
installer instructions and checks the `desktop-linux` context, Compose and container execution.
It does not silently accept Docker's license terms or change Windows virtualization settings.

The Windows shell uses C# and WinUI 3 with native Fluent controls. SwiftUI remains the macOS UI.
Linux UI selection is deferred. Engine behavior stays
in Go and actions use the same versioned JSON CLI/API contracts on every platform.

## Native implementation

- Windows core startup uses a detached process and writes stdout/stderr to the instance log.
- Graceful stop uses a Windows named event scoped to the logon session, instance and process;
  shutdown calls the same `Core.Close` as Unix. A missing event refuses to stop a reused/unowned
  PID. Unix keeps its existing process-session and signal implementation.
- Windows private directories use protected, inheritable current-user/SYSTEM ACLs; Unix
  retains its POSIX permissions. Native CI checks ACL inheritance.
- Instance data: `%LOCALAPPDATA%\Stormo\Instances\default`, outside the install directory.
- The app forces local target, its explicit instance and Docker Desktop's Linux context. It
  clears inherited remote Docker and core-bind overrides. Core API reads stay on loopback.
- Windows host bind paths in Compose use forward slashes, retaining drive letters/spaces;
  container paths remain Linux paths. Docker Desktop supplies `host.docker.internal` without
  an override to the Linux VM gateway.
- The development installer bundles tracked source so the engine can build its Linux sidecar
  using Docker. A tagged release must use a matching published sidecar image instead.

The earlier separate Stormo WSL prototype is superseded. This app does not unregister it or
copy/delete its data; any tester with data in that prototype must preserve it and migrate it
explicitly. No automatic migration is claimed.

## Validation and remaining gates

CI builds the actual Windows executable, runs native Go and C# runtime contract tests and exercises create
instance → detached core up → identity check → refusal to stop another workspace → graceful
core down → restart, using Windows paths with spaces. It publishes a self-contained WinUI desktop, builds and installs the per-user setup EXE, then
launches the installed app and captures Setup/Agents/Core in both system themes through Windows
UI Automation.
Linux CI continues to cover the existing engine. Packaging success is not clean-PC acceptance.

Still required: clean Windows 11 + Docker Desktop acceptance, container-to-core networking,
Windows filesystem bind/write behavior, agent/provider onboarding parity, signing, update and
repair UX, dependency distribution compliance, and release/tag/sidecar version wiring.
Uninstall must preserve instances. Deleting user data is always a separate explicit action.

See [build/setup instructions](../../apps/windows/README.md) and [shared parity](README.md).

Platform references: [Windows process flags](https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags),
[Docker Desktop networking](https://docs.docker.com/desktop/features/networking/networking-how-tos/),
[Windows file sharing](https://docs.docker.com/desktop/setup/install/windows-permission-requirements/).
