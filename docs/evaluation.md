# Public evaluation

The public evaluation suite is deliberately small and inspectable. It measures
retrieval against Contextmaxxer's own source tree, so it is useful for regression
testing and pipeline experiments, not as independent evidence that the system
generalizes to arbitrary repositories.

## Dataset boundary

`internal/eval/testdata/manifest.public.json` contains one project:

- 18 development queries in `queries.contextmaxxer.gen.json`
- 12 reserved queries in `queries.contextmaxxer.gen.holdout.json`

The development split may be used while changing retrieval. Treat the reserved
split as a final check; repeated tuning against it turns it into another
development set.

Private-project cases used during development are intentionally excluded from
the public repository. Published results must state which suite and split they
use rather than combining private and public numbers.

## Reproduce locally

From the repository root:

```text
task bootstrap
task build
.task/build/contextmaxxer index .
go build -o .task/build/eval ./cmd/eval
.task/build/eval --manifest internal/eval/testdata/manifest.public.json
.task/build/eval --manifest internal/eval/testdata/manifest.public.json --holdout
```

On Windows, use the `.exe` suffix for both binaries. The first full run downloads
the default embedding model, reranker and ONNX Runtime.

Record the commit, OS, CPU, model names and full command with every result. Do
not compare numbers produced from different manifests or splits as if they were
the same benchmark.

## Larger benchmark

[BENCHMARK.md](../BENCHMARK.md) documents the pinned external repositories,
baselines, negative results and CORE-Bench experiments. Claims there have a
different evaluation boundary from this self-eval and should remain separate.
