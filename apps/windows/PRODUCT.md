# Stormo Windows desktop
<!-- impeccable:product-schema 1 -->

## Platform
adaptive

## Users and purpose
Stormo owners on Windows, including people who do not know Docker. Install the app, complete
guided runtime setup, and operate their local agents without a terminal.

## Stack
Qt 6 / PySide6, selected to support Windows first and a later Linux desktop. The Go engine runs natively as stormo.exe on Windows. User confirmed guided Docker Desktop setup for the initial version.
The installer bundles Python, Qt and the Windows engine. Docker Desktop runs the Linux agent containers.

## Constraints
Preserve the CLI/API contract used by macOS. Local only. Shared parity ledger in docs/apps.
Windows 11 x64 is the initial implementation target; wider OS support remains unverified.
The current slice is setup, a default workspace, core controls and the agent table; do not
represent it as macOS feature parity. Signing and clean-Windows acceptance remain release gates.
