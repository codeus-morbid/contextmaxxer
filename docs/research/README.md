# Research index

Contextmaxxer's research notes are kept separate from the supported product
path. They document evidence, caveats and abandoned approaches; they do not
promise that experimental models or training artifacts ship with releases.

- [Product benchmark and research log](../../BENCHMARK.md) — paired-agent
  comparisons, CORE-Bench evaluation and negative results
- [SWE-Explore, partial run](swe-explore.md) — external whole-pipeline
  benchmark against published BM25/TF-IDF/agent baselines; 81% of the set (pro and
  multilingual complete), with its sampling bias stated
- [Public evaluation](../evaluation.md) — the small checked-in regression suite
- [`cmd/corebench`](../../cmd/corebench) — public CORE-Bench runner
- [`cmd/exploreprobe`](../../cmd/exploreprobe) — SWE-Explore runner (budgeted scoring)
- [`cmd/embprobe`](../../cmd/embprobe) — runtime-level embedding smoke test
- [`cmd/ftdata`](../../cmd/ftdata) — training-pair extraction tooling

Fine-tuned weights, full training data and the local training workspace are not
currently published. Results that depend on them are labeled as recorded
research and must not be presented as independently reproducible until those
artifacts, provenance and a model card are released.
