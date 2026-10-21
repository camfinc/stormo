# Desktop app changes

- Update `docs/apps/README.md` in every change that adds, removes, or changes a desktop feature.
  Record platform differences explicitly. Planned features are not implemented features.
- Keep setup/build/known-limit instructions in the platform README; keep the cross-platform
  feature ledger in `docs/apps/README.md` as the single shared backlog.
- Reuse the Go CLI and versioned API contracts. Do not independently implement engine behavior
  or bypass its local/remote, instance ownership, configuration, or secret-handling guards.
- Report native-platform runtime verification separately from unit tests and cross-host renders.
  Packaging success does not prove clean-machine installation or runtime networking.
