# Contributing to Contextmaxxer

Contextmaxxer is in public beta. Focused bug reports, reproducible retrieval
misses, documentation fixes and small implementation PRs are welcome.

## Before opening an issue

- Search existing issues first.
- For incorrect search results, include the query, the expected symbol and the
  smallest repository or code sample that reproduces the miss.
- Do not attach proprietary source, raw feedback logs or credentials. Use
  `contextmaxxer feedback export . -redact` when sharing feedback data.
- Report vulnerabilities through the process in [SECURITY.md](SECURITY.md), not
  in a public issue.

## Development setup

Requirements:

- Go 1.25.x
- [Task](https://taskfile.dev/)
- Git, `rustup` and a C compiler
- Linux amd64: GCC
- Windows amd64: a MinGW-compatible GCC and the GNU Rust host toolchain

The native tokenizer is built from a pinned upstream commit and cached locally:

```text
task bootstrap
task test
go vet ./...
task build
```

Build output belongs under `.task/build`; generated libraries under `libs/` are
local build artifacts and must not be committed.

The default test suite performs no model downloads. Run the model-backed ONNX
integration tests explicitly when changing embedding runtime behavior:

```text
CONTEXTMAXXER_RUN_MODEL_TESTS=1 go test ./internal/embed
```

## Pull requests

- Keep each PR focused on one change.
- Add a regression test for every bug fix.
- Run formatting, tests, vet and a native build before opening the PR.
- Explain user-visible behavior and trade-offs in the description.
- Benchmark claims must include the exact command, dataset/split, hardware and
  raw output needed to reproduce them.

The project uses English for code, comments, documentation and commit messages.
