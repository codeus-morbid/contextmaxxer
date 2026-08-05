# Third-party notices

Contextmaxxer is MIT-licensed, but it incorporates and downloads third-party
software under its own terms. Exact Go dependency versions are recorded in
`go.mod` and `go.sum`.

The distributed binary includes:

- `github.com/daulet/tokenizers` — MIT
- `github.com/fsnotify/fsnotify` — BSD-3-Clause
- `github.com/google/uuid` — BSD-3-Clause
- `github.com/mark3labs/mcp-go` — MIT
- Tree-sitter Go bindings and the bundled language grammars — MIT
- `github.com/yalue/onnxruntime_go` — MIT
- `modernc.org/sqlite` — BSD-3-Clause

Test-only dependencies are not part of release binaries. Transitive dependency
versions and their upstream license files can be resolved from the module graph
with `go list -m all`.

At runtime, Contextmaxxer downloads ONNX Runtime from Microsoft and model files
from their upstream publishers. Those files are not relicensed by this project;
their upstream licenses and model cards apply. See the pinned URLs and checksums
in `internal/embed/model.go` and `internal/rerank/onnx.go`.

Release archives include this notice and the project's `LICENSE`. This document
is informational and does not replace the full upstream license texts.
