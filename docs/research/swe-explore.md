# SWE-Explore

**Status: 689 of 848 instances (81%). Two of the three sub-datasets are COMPLETE — pro (215/215) and multilingual (182/182); verified is 292 of 451, and every one of the 159 missing instances is django.** These numbers
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

689 instances (81% of the benchmark), B = 500, served defaults:

| | **Contextmaxxer** | BM25 | TF-IDF | Potion (RAG) | CoSIL | Claude Code | Oracle |
|---|---:|---:|---:|---:|---:|---:|---:|
| HitFile | **0.509** | 0.079 | 0.140 | 0.088 | 0.544 | 0.667 | 0.923 |
| Prec | **0.340** | 0.055 | 0.117 | 0.055 | 0.581 | 0.598 | 1.000 |
| Rec_l | **0.048** | 0.021 | 0.049 | 0.025 | 0.788 | 0.154 | 0.953 |
| HitRegion | **0.359** | 0.065 | 0.121 | 0.069 | 0.544 | 0.531 | 0.915 |

Baseline figures are the paper's, measured on all 848 instances; ours are on
the 689 subset. For pro and multilingual that is now the whole sub-dataset;
verified is missing 159 django instances, so the overall comparison is close to
but not yet exactly like-for-like.

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

### By sub-dataset

| Sub-dataset | Scored | Of | HitFile | Prec |
|---|---:|---:|---:|---:|
| **pro** | 215 | **215 (all)** | 0.426 | 0.340 |
| **multilingual** | 182 | **182 (all)** | 0.513 | 0.239 |
| verified | 292 | 451 | 0.569 | 0.402 |

`pro` is the hardest set by some way, and `multilingual` costs the most
precision — consistent with the per-language pattern below, since that is where
the non-Python repositories are.

The numbers are stable as the sample grows, which is the reason to trust them:
387 instances gave HitFile 0.522, 438 gave 0.531, and 689 give 0.509, while
precision moved 0.338 -> 0.342 -> 0.340 across the same span. The 159 django
instances still missing are unlikely to change the picture; they are needed to
remove the caveat, not to find the answer.

### Per repository, and a guess that did not survive

An earlier version of this note guessed that the subset understated the tool,
because the cheap-to-index repositories it favoured are small while the best
results seen so far were on large corpora. **Django was then indexed — 50
instances, the largest Python corpus in the set at ~22k symbols a snapshot —
and the guess did not hold.**

| Repository | Language | n | HitFile | HitRegion | Prec |
|---|---|---:|---:|---:|---:|
| scikit-learn | Python | 31 | 0.651 | **0.568** | **0.556** |
| matplotlib | Python | 30 | 0.640 | 0.532 | 0.444 |
| xarray | Python | 18 | 0.625 | 0.449 | 0.380 |
| django | Python | 50 | 0.600 | 0.399 | 0.373 |
| astropy | Python | 21 | 0.552 | 0.458 | 0.401 |
| vuls | Go | 21 | 0.546 | 0.327 | 0.351 |
| ansible | Python | 41 | 0.537 | 0.323 | 0.435 |
| sphinx | Python | 37 | 0.504 | 0.395 | 0.366 |
| sympy | Python | 66 | 0.497 | 0.412 | 0.406 |
| pytest | Python | 18 | 0.470 | 0.408 | 0.327 |
| qutebrowser | Python | 34 | 0.439 | 0.318 | 0.404 |
| NodeBB | JavaScript | 22 | 0.388 | 0.292 | 0.329 |
| openlibrary | Python | 21 | 0.381 | 0.218 | 0.186 |
| teleport | Go | 19 | 0.363 | 0.207 | 0.203 |

Repositories with fewer than 15 instances are omitted from the table as too
small to read; `requests` (n=8) sits at 0.639 HitFile on 35 code files, and
`preact` (n=11) at 0.458 with precision 0.084 — still the worst in the set even
after three extractor fixes.

django lands mid-table, above the overall average but well below scikit-learn,
which is a third its size, and below `requests` at a fiftieth of it. Corpus
size is not the driver. Language is the stronger signal: the top of the table
is Python throughout, and the JavaScript repositories sit at the bottom. Go is
split — vuls at 0.546 against teleport at 0.363 — which suggests the language
label is itself a proxy for something narrower, most likely how much of the
codebase is expressed as named calls rather than dispatch or composition.

## What the sweeps established

Two independent axes were measured over the 387 instances available at the time.

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

## What each stage of the pipeline is worth

Five configurations over the same 438 instances, varying retrieval only — no
reindexing, about fifteen minutes each.

| Configuration | HitFile | HitRegion | Prec | Rec_l | F1 | Lines |
|---|---:|---:|---:|---:|---:|---:|
| **Full (served default)** | 0.531 | 0.367 | **0.342** | 0.051 | 0.076 | 89 |
| No graph (`alpha=1`) | 0.529 | 0.362 | 0.322 | 0.051 | 0.075 | 94 |
| No intent ranker | 0.531 | 0.366 | 0.327 | 0.046 | 0.072 | 86 |
| No cross-encoder | 0.531 | **0.378** | 0.306 | **0.067** | **0.090** | 125 |
| Neither graph nor rerank | 0.532 | 0.374 | 0.313 | 0.067 | 0.091 | 124 |

**HitFile is 0.531 in every configuration.** Which files come back is decided
entirely by the seeds — lexical plus vector. The graph, PageRank, the
cross-encoder and the intent ranker add no file to the answer; they reorder
what the seeds already found. The ceiling on reach is therefore a seed-stage
property, and at 0.531 against Oracle's 0.923 that is where the headroom is.

**Everything downstream of the seeds buys +0.029 precision.** Full system
against bare seeds: 0.342 vs 0.313, while line recall is WORSE (0.051 vs 0.067)
and so is F1. Per stage: cross-encoder +0.036, graph +0.020, intent ranker
+0.015, each paid for in coverage.

**The cross-encoder is the most expensive stage and the most questionable
one.** It also dominates query latency. Without it HitRegion, line recall and
F1 all improve; only precision drops. Whether that trade is right depends on
the paper's own finding that context efficiency correlates with downstream
resolve rate at r = +0.950, above every recall measure — which argues for
keeping it.

One caveat keeps this from being a clean comparison: without reranking the
response carries 125 lines instead of 89, because reranking changes which
symbols occupy the six full-body slots and therefore how much code travels.
Part of the recall gain is simply more text.

**What this does not establish.** SWE-Explore queries are long issue reports,
dense with the vocabulary of the code they describe — the case where lexical
seeds are strongest. A short navigational query ("what runs when X happens")
gives them far less to match on, and the graph may well weigh differently
there. That has to be measured on agent-style traffic; it is not answered by
this benchmark.

## Where the ceiling actually is

Six levers left HitFile at ~0.53: the graph, the cross-encoder, the intent
ranker, a 10x seed pool, a fine-tuned embedder, and query shaping. That called
for a different question — not "how do we rank better" but "is the file even
reachable". Two diagnostic runs answer it (budget off, reranker off, since
HitFile is identical with and without reranking and a 500-line budget would
truncate the response long before rank 200):

| max_results | gold files found |
|---:|---:|
| 20 | 0.516 |
| 200 | 0.647 |

Then asking each index which gold files it physically contains gives the
ceiling: **0.790** (1778 of 2251 gold files across 493 instances). Together
those decompose the gap completely:

| Layer | Share of gold files | Nature |
|---|---:|---|
| **Found today** (max_results=20) | **51.6%** | |
| In the response but below rank 20 | 13.1% | depth of the returned list |
| In the index, not retrieved even at rank 200 | **14.3%** | the real retrieval gap |
| Test files, excluded by filename filter | **14.6%** | our own design decision |
| Not code: configs, documentation | 5.4% | by construction |
| Other (`target/` skip, generated, absent from snapshot) | 1.1% | known trade-offs |

51.6 + 13.1 + 14.3 = 79.0, which is the measured ceiling.

**The single largest loss is not weak retrieval — it is the test-file filter.**
At 14.6% it costs more than any other cause. Include tests and the ceiling
becomes 0.936, against Oracle's 0.923 in the paper. That agreement to the third
decimal is unlikely to be coincidence: the benchmark's ground truth is what
agents READ, and agents read tests, so its labels include them. Excluding tests
is right for the product — an agent rarely wants one — and is a straight
deduction here.

**The retrieval work worth doing is the 14.3%**: files that sit in the index and
are not retrieved even among two hundred results. Ranking cannot reach them and
neither can a bigger candidate pool; only better similarity can.

The indexer itself is clean. Of the code files missing from an index, only 0.6%
of gold are actually present on disk, and every example checked had a reason:
`tests/roots/test-ext-autodoc/target/*.py` sits under a directory named
`target`, which is skipped as a build artifact (Rust, Maven); `_spec.rb` matches
the test-name filter; `.pb.gw.go` is generated code.

## What was tried against the ceiling, and what it cost

Every idea below was measured on the same instances, and all but one failed.
They are recorded because the failures are informative: they say what kind of
work can move this and what cannot.

**Query shaping — the one that worked.** The benchmark hands over whole issue
reports, ~850 characters of prose, reproduction steps and headings. Searching
for the TITLE alone beats searching for all of it:

| Query | HitFile | HitRegion | Prec | Rec_l | F1 |
|---|---:|---:|---:|---:|---:|
| raw issue | 0.515 | 0.356 | 0.334 | 0.050 | 0.075 |
| **title** | 0.513 | 0.361 | **0.392** | **0.068** | **0.099** |
| identifiers only | 0.434 | 0.299 | 0.301 | 0.052 | 0.077 |
| title + identifiers | **0.520** | 0.361 | 0.380 | 0.063 | 0.094 |

+0.058 precision from deleting text, larger than any component of the pipeline
is worth. Identifiers alone are worse than either: a bag of names loses the
sentence the vector needs. Note this says nothing about reach — HitFile does
not move.

**A fine-tuned embedder — small and real.** ft2 over 108 snapshots in five
languages: precision +0.031 (t = 2.30, 95% CI +0.005..+0.058), winning on 61
and losing on 41. It buys about what the cross-encoder buys, and like
everything else leaves HitFile alone (0.492 -> 0.494). Its three losses are the
smallest corpora in the set, matching the earlier record that ft2 helps only on
large ones.

**Anchor expansion — failed, and the failure is the useful part.** Of the gold
files we miss that ARE indexed, 64.5% sit one or two call-graph hops from a
file we returned; LARGER reports this mechanism as its single largest gain.
Four variants were measured:

| Variant | HitFile | Prec |
|---|---:|---:|
| off | **0.497** | **0.354** |
| neighbours added to the candidate pool | 0.499 | 0.309 |
| anchored on fused candidates, 10 neighbours | 0.480 | 0.332 |
| same, 20 neighbours | 0.466 | 0.316 |
| requiring 2 anchors to agree | 0.502 | 0.334 |

Monotonically worse as it widens. **Reachability is not discriminability**: an
anchor has hundreds of neighbours, the gold file is among them, and a call
graph offers nothing to tell them apart. Anchor agreement removes the harm and
adds no benefit. LARGER resolves the choice with GPT-5.2 inside the search
loop — which is exactly the cost this tool exists to avoid.

**Seed pool size — no effect at all.** 20 -> 200 candidates per channel moved
HitFile 0.531 -> 0.526. The pool was never the constraint.

**Indexing test files — the largest reachable loss, and it changes nothing.**
Tests are 14.6% of all gold files, the biggest single cause of unreachability,
and including them would lift the ceiling from 0.790 to 0.936, next to Oracle's
0.923. Measured on 30 snapshots chosen because their gold contains tests (21.0%
of their gold files are tests), indexing them side by side with --include-tests:

| | without tests | with tests |
|---|---:|---:|
| HitFile | 0.392 | 0.409 |
| HitRegion | 0.269 | 0.286 |
| Prec | 0.301 | 0.306 |

+0.018 HitFile, t = 1.39, 95% CI -0.007..+0.043 — not significant. Better on 2
instances, worse on none, unchanged on 28. Precision did not fall, so the
feared cost (test code competing lexically with implementation) did not
materialise either.

The ceiling rose by 21 points for this subset and the answer moved by under
two. **Removing the largest cause of unreachability did not help, because what
is retrieved is not limited by what is present.**

## What this benchmark does not measure

SWE-Explore asks one question: given an issue report, name every region a
solver had to read. That is adjacent to what this tool is for, and the
difference shows up sharply when both are measured on the same day.

`cmd/chainprobe` measures the other task — find a mechanism, then follow the
call chain — over hand-traced chains on three repositories:

| Repository | Hops | Arrived via graph | As a sibling result | Missed | Followable |
|---|---:|---:|---:|---:|---:|
| cockroach | 9 | 9 | 0 | 0 | **1.00** |
| django | 13 | 10 | 1 | 2 | 0.85 |
| postgres | 17 | 17 | 0 | 0 | **1.00** |
| **total** | **39** | **36** | **1** | **2** | **0.95** |

**Source retrieval is 1.000 on all three.** The queried symbol is found every
time, against HitFile 0.53 here — and 36 of 39 next hops arrive attached to the
previous answer, so the agent pays for no second search.

So the ablation result above — "the graph adds no file to the answer" — is
true of issue localization and false of navigation. On this benchmark the query
is 850 characters of prose and lexical seeding already reaches what the graph
would; on a short mechanism query the graph carries the chain outright. The two
numbers are not in conflict, they are about different questions, and the gap
between 1.000 and 0.53 is the same query-shape effect measured in the sweep
above, in its extreme form.

Read the tables here as what this tool costs and returns when handed a bug
report, not as what it does when an agent navigates code.

## The boundary, stated

Six independent attempts to lift reach, all measured on the same instances, all
null. They are worth listing together because the pattern is the finding:

| Attempt | Δ HitFile | Verdict |
|---|---:|---|
| Seed pool 20 -> 200 | -0.005 | pool was never the constraint |
| Anchor expansion over the graph | -0.031 | monotonically worse as it widens |
| Fine-tuned embedder (ft2) | +0.002 | +0.031 precision, no reach |
| Indexing test files | +0.018 | t = 1.39, ceiling +21pp and answer +1.8pp |
| Graph context in embedded text | -0.010 | better on 0 of 30 instances |
| Confidence-gated escalation | — | **no signal to gate on** |

The last one closes the most promising route. Escalating to a model only on
uncertain queries would preserve the product's premise — no model at query
time, most of the time — but it needs the server to know when it is wrong, and
it does not:

- Queries the server answers **confidently score 0.505**; ones it flags as
  uncertain score **0.510**. Confidence is anti-correlated with being right, by
  a hair.
- `high` and `medium` never occurred at all across 689 instances: on issue text
  the system is always "unsure", which makes the scale meaningless.
- The continuous form is barely better: the top1-top2 score gap correlates with
  file recall at **r = 0.109**, and 67% of queries fall in the lowest bucket, so
  "escalate when unsure" would escalate two thirds of all traffic.

That confirms at n=689 what a smaller earlier measurement had recorded as false
confidence between 0.42 and 0.67.

**Where the boundary actually is.** Reach is a property of the seed stage —
HitFile is 0.531 with the graph, the cross-encoder and the intent ranker all
disabled, the same as with everything on. It is not ranking, and it is not
pool size. 14.3% of gold sits in the index and is never retrieved at rank 200,
because vector and lexical similarity cannot separate those files from
plausible neighbours. Nothing local moves that: the methods that do (LARGER,
CoSIL) put an LLM inside the search loop, which is the cost this tool exists to
avoid.

**What that does not mean.** The same graph that adds nothing here carries 0.95
of hops on navigation, and source retrieval there is 1.000. The boundary is
specific to answering an 850-character bug report in one shot, which is not the
job this tool was built for.

## Limits

- **81% of the benchmark.** Indexing all 847 snapshots is 48-64 hours of GPU;
  the 159 instances still missing are all django, ~24 hours on one GPU.
- **The subset over-represents Python**, which is also where the tool scores
  best, so the overall averages are probably flattered rather than understated.
  sympy and ansible are still absent. (The earlier worry ran the other way —
  that omitting large repositories understated the tool — and the django run
  settled it: size is not the driver, language is.)
- Baselines come from the paper and were run on the full set.
- **The scoring metric itself was wrong twice** during this work — once
  producing nDCG above 1, once crediting coverage of lines the response never
  sent — and the source data was corrupt a third time: 16 of 848 snapshots held
  a duplicate copy of their own tree, which doubles the symbols and collapses
  the call graph, since edge resolution requires a unique name. All three are
  fixed, the affected snapshots reindexed, and the fixes carry tests. The
  history is still a reason to treat any single number here as provisional.

## Reproducing

The data must be fetched from Hugging Face
(`SWE-Explore-Bench/SWE-Explore-Bench`) and one index built per snapshot:

    contextmaxxer index <repos>/<instance_id>
    exploreprobe -manifest manifest.jsonl -repos repos -budget 500

`-full` and `-max` reproduce the sweeps above.
