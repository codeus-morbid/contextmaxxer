# Changelog

All notable changes to Contextmaxxer are documented here. The project follows
[Semantic Versioning](https://semver.org/) once public tags are published.

## Unreleased

### Added

- **find_context answers a position.** A query shaped like `path/to/file.go:142`
  — or a whole `rg -n` output line pasted verbatim — is looked up rather than
  searched, returning the symbol enclosing that line with its callers and
  callees, and running no embedding or reranking. It is the door an agent
  holding a grep hit had no way to open before.
- **A post-grep hook** that hands that position back after a search, once per
  session, and only when the output carries a concrete path.
- **`contextmaxxer feedback adoption`** reports whether agents reach for the
  tool and whether the nudge changes what they do, counted from what the hooks
  and the server observed rather than from self-report. Telemetry is written
  beside the feedback log and holds no query text.
- Reproducible pinned tokenizer bootstrap for Windows amd64 and Linux amd64.
- Native per-platform CI and GitHub release packaging with checksums.
- Public self-evaluation manifest and evaluation guide.
- Security policy, contribution guide, issue forms and third-party notices.
- Product-first README and a terminal demo backed by a checked-in eval case.

### Changed

- Answers reserve their top slots for non-test files when the index holds
  name-conventioned tests. Measured free: precision up, file recall unchanged on
  every one of 124 snapshots. A no-op on an index without such tests.
- Public module namespace moved to `github.com/codeus-morbid/contextmaxxer`.
- Model-backed ONNX tests are explicit opt-in integration tests; the default
  suite is hermetic and performs no model downloads.
- Product benchmark claims are separated from unpublished research artifacts.

### Fixed

- **Hooks never ran on Windows.** A hook command is executed through a shell,
  which eats the backslashes in a Windows path, so every hook this installer
  wrote died at "command not found" with a clean exit code — no discovery gate
  ever fired. Separators are now normalized, and re-running `init` repairs an
  existing broken config instead of treating it as already wired.
- **The discovery gate no longer traps an agent.** It stays silent in a
  repository that has no index, and blocks at most three searches rather than
  every one: the marker that opens it is written by a PostToolUse hook, and that
  hook does not run when find_context FAILS, so a corrupt index used to leave an
  agent with neither search tool.
- `goldengate` now defaults to a checked-in public suite instead of a private
  local fixture that is absent from fresh clones.

### Removed

- Private project evaluation corpora, local agent configuration and internal
  planning documents from the future public snapshot.
- The non-working `CGO_ENABLED=0` GoReleaser configuration.
