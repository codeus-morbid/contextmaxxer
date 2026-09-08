# Research index

Contextmaxxer's research notes are kept separate from the supported product
path. They document evidence, caveats and abandoned approaches; they do not
promise that experimental models or training artifacts ship with releases.

- [Product benchmark and research log](../../BENCHMARK.md) — paired-agent
  comparisons, CORE-Bench evaluation and negative results
- [SWE-Explore, complete](swe-explore.md) — external whole-pipeline benchmark
  against published BM25/TF-IDF/agent baselines, all 848 instances, with the
  ceiling decomposed and every failed lever recorded
- [Public evaluation](../evaluation.md) — the small checked-in regression suite
- [`cmd/corebench`](../../cmd/corebench) — public CORE-Bench runner
- [`cmd/exploreprobe`](../../cmd/exploreprobe) — SWE-Explore runner (budgeted scoring)
- [`cmd/chainprobe`](../../cmd/chainprobe) — how much of a call chain arrives
  without a second search, and what the same hops cost as a text search
- [`cmd/regionprobe`](../../cmd/regionprobe) — where a missed gold region
  actually sits relative to what was returned
- [`cmd/reachprobe`](../../cmd/reachprobe) — why a gold file is never retrieved:
  vocabulary, weighting, or indexer coverage
- [`cmd/embprobe`](../../cmd/embprobe) — runtime-level embedding smoke test
- [`cmd/ftdata`](../../cmd/ftdata) — training-pair extraction tooling

The `ft2` weights and their model card are published as
[jina-v2-code-ft2](https://huggingface.co/codeusmorbid/jina-v2-code-ft2) under
CC BY-NC-SA 4.0, the licence of the CORE-Bench training data. The full training
data and the local training workspace are not published, so results depending
on them are recorded research: the weights and the recipe can be inspected, the
training run cannot yet be repeated end to end from this repository alone.
