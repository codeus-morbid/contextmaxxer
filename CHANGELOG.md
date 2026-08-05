# Changelog

All notable changes to Contextmaxxer are documented here. The project follows
[Semantic Versioning](https://semver.org/) once public tags are published.

## Unreleased

### Added

- Reproducible pinned tokenizer bootstrap for Windows amd64 and Linux amd64.
- Native per-platform CI and GitHub release packaging with checksums.
- Public self-evaluation manifest and evaluation guide.
- Security policy, contribution guide, issue forms and third-party notices.
- Product-first README and a terminal demo backed by a checked-in eval case.

### Changed

- Public module namespace moved to `github.com/codeus-morbid/contextmaxxer`.
- Model-backed ONNX tests are explicit opt-in integration tests; the default
  suite is hermetic and performs no model downloads.
- Product benchmark claims are separated from unpublished research artifacts.

### Fixed

- `goldengate` now defaults to a checked-in public suite instead of a private
  local fixture that is absent from fresh clones.

### Removed

- Private project evaluation corpora, local agent configuration and internal
  planning documents from the future public snapshot.
- The non-working `CGO_ENABLED=0` GoReleaser configuration.
