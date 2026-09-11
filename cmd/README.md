# Commands

One of these is the product. The rest are measurement tools kept in the
repository because the numbers in [BENCHMARK.md](../BENCHMARK.md) and
[docs/research](../docs/research) are only worth reading if the thing that
produced them is readable too.

## The product

| | |
|---|---|
| [`contextmaxxer`](contextmaxxer) | the binary a user installs: index, warmup, init, query, mcp, hook, feedback |

Everything below builds only when asked and ships in no release archive.

## Benchmark runners

| | |
|---|---|
| [`exploreprobe`](exploreprobe) | SWE-Explore under the paper's protocol (B = 500 budgeted scoring) |
| [`corebench`](corebench) | CORE-Bench, embedder and seed-fusion stages |
| [`mcpeval`](mcpeval) | the evaluation corpus driven through a real MCP server over JSON-RPC, to catch a tuned configuration that never reached the served path |
| [`eval`](eval) | the small public regression suite |
| [`goldengate`](goldengate) | golden-output guard |

## Probes that answer one question each

| | |
|---|---|
| [`chainprobe`](chainprobe) | how much of a call chain arrives without a second search, and what the same hops cost as a text search |
| [`regionprobe`](regionprobe) | where a missed gold region sits relative to what was returned |
| [`reachprobe`](reachprobe) | why a gold file is never retrieved: vocabulary, weighting, or indexer coverage |
| [`rspbreak`](rspbreak) | what the response is spending its tokens on |
| [`negprobe`](negprobe) | behaviour on queries with no right answer |
| [`selfsweep`](selfsweep) | self-evaluation across configurations |
| [`giteval`](giteval) | cases derived from repository history |
| [`deepprobe`](deepprobe), [`lateexp`](lateexp) | deep reranking and late interaction, both measured and parked |
| [`embprobe`](embprobe) | embedding runtime smoke test |
| [`idxstats`](idxstats), [`diag`](diag) | index and environment inspection |
| [`confcal`](confcal) | confidence calibration |
| [`soak`](soak) | long-running stability |
| [`ftdata`](ftdata) | training-pair extraction for the fine-tuning experiments |
| [`askctx`](askctx) | one-shot query helper |

Several of these need corpora that are not in this repository — SWE-Explore is
CC-BY-NC-ND and CORE-Bench is CC-BY-NC-SA, so the numbers are published and the
data is not. Those runners will build and refuse to run without it.
