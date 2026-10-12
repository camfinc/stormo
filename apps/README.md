# Stormo desktop apps

- [macOS](macos/README.md): SwiftUI app and StormoKit.
- [Windows preview](windows/README.md): Qt desktop, guided Docker Desktop/WSL setup and installer.
- [Shared feature ledger](../docs/apps/README.md): current parity and the review checklist.

Windows is the current porting priority. Linux desktop packaging is deferred; the Windows Qt
shell and protocol adapter are kept separate from WSL-specific runtime setup for future reuse.
