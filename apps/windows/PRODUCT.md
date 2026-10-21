# Stormo Windows desktop
<!-- impeccable:product-schema 1 -->

## Platform
adaptive

## Users and purpose
Stormo owners on Windows, including people who do not know Docker. Install the app, complete
guided runtime setup, and operate their local agents without a terminal.

## Stack
C# and WinUI 3 with standard Fluent controls, matching Windows conventions. The owner explicitly
selected native platform UI, as on macOS; cross-platform UI reuse is not a goal. The Go engine
runs natively as stormo.exe. Guided Docker Desktop setup remains the initial container path.
The per-user installer bundles .NET, Windows App SDK and the Windows engine.

## Constraints
Preserve the CLI/API contract used by macOS. Local only. Shared parity ledger in docs/apps.
Windows 11 x64 is the initial implementation target; wider OS support remains unverified.
The current slice is setup, a default workspace, core controls and the agent list; do not
represent it as macOS feature parity. Signing and clean-Windows acceptance remain release gates.
