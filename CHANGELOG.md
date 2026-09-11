# Changelog

All notable changes to Contextmaxxer are documented here. The project follows
[Semantic Versioning](https://semver.org/) once public tags are published.

## 0.1.1 - 2026-09-12

### Added

- **macOS arm64 archives.** The ONNX Runtime spec and the cgo directives were
  already in place; the tokenizer bootstrap was pinned to Linux in three places
  and now derives them from the host. Intel macOS exits with the reason rather
  than a download error later: upstream publishes exactly one darwin asset and
  it is arm64. macos-14 runs in CI as well as in release, so a break surfaces
  on a pull request rather than on a tag.
- **find_related_edits.** Given the diff just made, it reports where else the
  changed names already live, ranked by rarity. An agent handed the gold file
  paths resolves 84% of the measured SWE-bench Verified instances against 68%
  for one that searches for itself, and the file it misses has usually already
  been returned to it.

### Changed

- The tuning parameters are off find_context's published schema and behind
  `CONTEXTMAXXER_EXPERIMENTAL_TOOLS`. They were 42% of the schema every session
  carries, describing knobs the tool tells an agent to omit; passing one still
  works.
- The opening claim drops its speed ratios. They came from single paired runs
  whose repeats vary by up to 57%, and latency is roughly 2% of an agent's wall
  time. Zero source-file reads, which reproduces in every run, stays.
- The CORE-Bench table now says that our row is a retriever fused with BM25
  while every paper row is a retriever alone. Stripped to the encoder, the
  shipped default ties CodeRankEmbed at 0.122 rather than passing it at 0.150.
- SWE-Explore is reported at the served default of five results as well as at
  the benchmark's list of twenty: 0.342 file coverage against 0.529.

### Fixed

- The test floor could not see a suite laid out by directory. Files named
  models.py or tests.py inside tests/ are 11% of indexed files across 63
  projects and 57% of django's, and they competed for the slots the floor
  exists to protect. Excluding them from the index was implemented and
  reverted: 8.4% of SWE-Explore's gold files live there.
- Both bootstrap scripts reinstalled the pinned Rust toolchain on every build,
  testing rustup's output against a pattern that could not match it.

## 0.1.0 - 2026-09-08

First public release. Prebuilt binaries for Linux and Windows amd64 are attached
to this tag; the source-build path in INSTALL.md remains for every other target.

### Added

- **A release is now gated on the binary actually starting.** CI and the
  release workflow run the built binary through a real MCP handshake over
  stdio and assert it answers `initialize` and `tools/list`. The hermetic test
  suite never loads a real model, so a Go binding that asks for a newer ONNX
  Runtime API than the pinned native library provides used to compile clean,
  pass every test, and die at startup.
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

- **SWE-Explore is reported at the served default as well as at the benchmark's
  list of 20.** The table was labelled "served defaults" and was not: the runner
  asks for 20 results, the product serves 5, and the B=500 budget never bound
  (20 results send 89 lines). Both columns are now published — file coverage
  0.529 against 0.342, precision 0.338 against 0.396 — measured in one sitting
  with the same binary, the 20-result arm reproducing the published figures to
  three decimals.
- Answers reserve their top slots for non-test files when the index holds
  name-conventioned tests. Measured free: precision up, file recall unchanged on
  every one of 124 snapshots. A no-op on an index without such tests.
- Public module namespace moved to `github.com/codeus-morbid/contextmaxxer`.
- Model-backed ONNX tests are explicit opt-in integration tests; the default
  suite is hermetic and performs no model downloads.
- Product benchmark claims are separated from unpublished research artifacts.

### Fixed

- **`--watch` ignored ten extensions the indexer parses.** Changes to `.mjs`,
  `.cjs`, `.cs`, `.php`, `.kt`, `.kts`, `.sc`, `.scala`, `.cxx` or `.hh` never
  triggered a reindex, so the watcher reported itself running while the served
  index went stale. The watcher kept a second list of source extensions beside
  the indexer's; it now reads the indexer's table directly and a test asserts
  the two cannot drift again.
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
