# Windows desktop direction

Mode: Operate. Use genuine WinUI 3 / Fluent components, as explicitly selected by the owner.
The design baseline is Windows 11 Settings: calm system typography, native NavigationView,
standard spacing, restrained surfaces, system light/dark mode and high-contrast resources.
Preserve Stormo's approved app icon. Do not skin generic cross-platform widgets to mimic Windows.

First viewport: Mica window frame, left navigation (Setup, Agents, Core), page heading and
plain-language introduction, completed prerequisite rows, and one next-step panel. One primary
action advances setup; Check again remains separate. No decorative metrics or invented agents.

Use standard InfoBar for recoverable errors, CommandBar and ListView for fleet actions,
ContentDialog with Cancel as default for interruptions, and Expander for operation details.
Agent name/model/unit and state/health are grouped for scanning. The native empty-state InfoBar
states the preview's actual limits. Windows supplies keyboard focus, control motion, fonts,
accessibility roles and theme colors. Content scrolls and NavigationView collapses below 900 px; no free-form footer text in its icon rail.
Technical setup failures live in a native expander, keeping the primary instructions actionable.

Async operations keep the UI responsive, prevent duplicate actions and block close until done.
The progress bar describes ongoing work; errors remain visible with recovery guidance. Selection
is keyed by agent ID and cleared when that agent disappears. Closing normally keeps the core alive.
