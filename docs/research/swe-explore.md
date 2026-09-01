# SWE-Explore: partial run

**Status: interim. 387 of 848 instances (45%), not a full run.** These numbers
are a research record, not a product claim, and the sample is biased in a way
that matters — see [Limits](#limits) before quoting anything here.

[SWE-Explore](https://arxiv.org/html/2606.07297v1) grades the whole retrieval
pipeline rather than its entrance: given an issue and a repository snapshot, an
explorer returns a ranked list of `(file, start, end)` regions, scored against
line-level ground truth distilled from the trajectories of agents that actually
solved the task. That is nearer to what this tool is for than a diff-derived
label — we do not predict the patch, we hand the agent what it has to read
first.

The dataset is **CC-BY-NC-ND**: numbers may be published, the data may not be
redistributed, and no part of it is checked into this repository.

## Protocol

Scoring follows the paper: every baseline is reported at a **line budget
B = 500**, over the longest prefix of the response whose cumulative visible
lines fit the budget. Our runner is [`cmd/exploreprobe`](../../cmd/exploreprobe).

The paper's column names collide with ours, so they are restated here:

| Paper | Definition | Our name |
|---|---|---|
| HitFile | share of gold files reached | File recall |
| Prec | share of returned lines that are gold | Efficiency |
| Rec_l | share of gold lines covered | Line recall |
| HitRegion | share of gold regions with an overlapping prediction | — |

**nDCG is not reported here.** Our implementation of their formula produces
0.047 while every other metric sits several times above BM25, which is a
contradiction rather than a result — the paper reports Oracle at 0.858 instead
of 1.000, so their normalisation is not pinned down by the description. The
other four metrics are unambiguous and are not hedged.

## Results

387 instances (verified 147, pro 126, multilingual 114), B = 500, served
defaults:

| | **Contextmaxxer** | BM25 | TF-IDF | Potion (RAG) | CoSIL | Claude Code | Oracle |
|---|---:|---:|---:|---:|---:|---:|---:|
| HitFile | **0.522** | 0.079 | 0.140 | 0.088 | 0.544 | 0.667 | 0.923 |
| Prec | **0.338** | 0.055 | 0.117 | 0.055 | 0.581 | 0.598 | 1.000 |
| Rec_l | **0.051** | 0.021 | 0.049 | 0.025 | 0.788 | 0.154 | 0.953 |
| HitRegion | **0.363** | 0.065 | 0.121 | 0.069 | 0.544 | 0.531 | 0.915 |

Baseline figures are the paper's, measured on all 848 instances; ours are on
the 387 subset, so the comparison is indicative rather than like-for-like.

Contextmaxxer is several times above every non-agentic method and level with
CoSIL on file reach. [CoSIL](https://www.arxiv.org/abs/2503.22424v1) (ASE 2025)
is the closest comparison in kind, because it localises from a call graph too.
It builds that graph by static parsing, not by prompting, but puts the LLM
*inside the search loop*: a module-level pass picks candidate files, then a
function-level pass walks the function call graph for up to ten iterations with
top-5 pruning, one model call per step. The graph is built per issue and
nothing carries over to the next question. Here the cost is paid once at
indexing time and a query needs no model at all. The agents remain ahead on
precision.

Line recall is the weak column, and the sweeps below explain why.

## What the sweeps established

Two independent axes were measured over the same 387 instances.

**Full bodies per response** ([`-full`](../../cmd/exploreprobe)):

| Bodies | Lines sent | HitRegion | Prec | Rec_l | F1 |
|---:|---:|---:|---:|---:|---:|
| 3 (old default) | 62 | 0.360 | 0.334 | 0.034 | 0.056 |
| **6 (current)** | 90 | 0.363 | **0.338** | 0.051 | 0.076 |
| 12 | 155 | 0.367 | 0.308 | 0.076 | 0.101 |
| 20 (all) | 251 | 0.373 | 0.253 | 0.096 | 0.112 |

Six dominates three on **both** axes — it is not a tradeoff — which is why the
default moved. The genuine tradeoff starts after six.

**Results per response** (`-max`, rerank pool matched):

| max_results | Lines sent | HitFile | HitRegion | Prec | Rec_l |
|---:|---:|---:|---:|---:|---:|
| **20 (current)** | 90 | 0.522 | 0.363 | **0.338** | **0.051** |
| 40 | 113 | 0.578 | 0.387 | 0.261 | 0.046 |
| 60 | 141 | **0.607** | **0.399** | 0.220 | 0.047 |

More results reach more files and cost a third of the precision, and line
recall does not move at all — the extra results arrive compacted, so forty of
them carry 113 lines against twenty carrying 90. **The lever is how many bodies
travel, not how many results.** The paper's own correlation analysis puts
context efficiency at r = +0.950 with downstream resolve rate, above every
recall measure, so trading precision for reach is the wrong direction.

A 500-line budget filled to 90 lines is therefore not an packing defect: there
is nothing to add without paying precision for it.

## Limits

- **45% of the benchmark.** Indexing all 847 snapshots is 48-64 hours of GPU;
  django alone is 209 instances and ~23 hours of it.
- **The subset is biased toward small and mid-sized repositories** — they were
  chosen because they index cheaply. django, sympy and ansible are absent, and
  the tool's strongest observed results are on large corpora (all three lucene
  instances of the pilot scored file recall 1.00), so the subset is likely
  unflattering rather than flattering. That is a guess, not a measurement.
- Baselines come from the paper and were run on the full set.
- The scoring metric itself was wrong twice during this work — once producing
  nDCG above 1, once crediting coverage of lines the response never sent. Both
  are fixed and covered by tests, but the history is a reason to treat any
  single number here as provisional.

## Reproducing

The data must be fetched from Hugging Face
(`SWE-Explore-Bench/SWE-Explore-Bench`) and one index built per snapshot:

    contextmaxxer index <repos>/<instance_id>
    exploreprobe -manifest manifest.jsonl -repos repos -budget 500

`-full` and `-max` reproduce the sweeps above.
