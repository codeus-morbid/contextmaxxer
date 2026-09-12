# SWE-Explore

**Status: COMPLETE — 848 of 848 instances. All three sub-datasets are done: pro (215), multilingual (182), verified (451).** These numbers
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

All 848 instances, B = 500. The runner asks for a list of 20; the served
default is 5. The paper fixes K = 5 for every explorer it compares ("each
explorer is asked to return its five most relevant regions"), so the
served-default row below the table is the one to read against these baselines,
and the table itself shows what a longer list reaches. Both are reported.

| | **Contextmaxxer** | BM25 | TF-IDF | Potion (RAG) | CoSIL | Claude Code | Oracle |
|---|---:|---:|---:|---:|---:|---:|---:|
| HitFile | **0.529** | 0.079 | 0.140 | 0.088 | 0.544 | 0.667 | 0.923 |
| Prec | **0.339** | 0.055 | 0.117 | 0.055 | 0.581 | 0.598 | 1.000 |
| Rec_l | **0.047** | 0.021 | 0.049 | 0.025 | 0.788 | 0.154 | 0.953 |
| HitRegion | **0.375** | 0.065 | 0.121 | 0.069 | 0.544 | 0.531 | 0.915 |

Served default (`-max 5`, everything else identical), same 848 instances, same
binary, same sitting:

| | HitFile | Prec | Rec_l | HitRegion | Visible lines |
|---|---:|---:|---:|---:|---:|
| list of 20 (table above) | 0.529 | 0.338 | 0.047 | 0.375 | 89 |
| **served default, 5** | 0.342 | **0.396** | 0.036 | 0.267 | 58 |

A third of the file coverage buys a sixth more precision and a third fewer
lines. It also moves the first useful result from rank 2.69 to 1.70. The
list-of-20 arm of this pair reproduced the published table to three decimals
(Prec 0.338 against the recorded 0.339), so the two rows differ only in how
many results were requested.

**Both sides are now the same 848 instances**, so the comparison is exactly
like-for-like and the earlier subset caveat is gone.

Two controls stand behind the merge, because the run was assembled over weeks
rather than in one pass. The later django instances were scored with a newer
build, so five rows already in the old set were re-scored with it first: all
five reproduced to the last digit. And every one of the 848 indexes was checked
against its repository's median size before scoring — which caught exactly one
snapshot, `django__django-16662`, whose archive had been truncated at extraction
(17 MB against 218, everything after `django/` missing). It was re-fetched and
re-indexed. One in 848 is the failure rate of the harness's own extraction
check, which judges success by "the extracted directory is not empty".

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
| **verified** | 451 | **451 (all)** | 0.584 | 0.378 |

`pro` is the hardest set by some way, and `multilingual` costs the most
precision — consistent with the per-language pattern below, since that is where
the non-Python repositories are.

The numbers were stable the whole way up, which is the reason to trust them:
387 instances gave HitFile 0.522, then 438 gave 0.531, 689 gave 0.509, 780 gave
0.523 and the complete 848 give 0.529 — while precision sat at 0.338, 0.342,
0.340, 0.340, 0.339 across the same span. Precision moved by 0.004 in total
while the sample more than doubled, and the last 68 instances moved HitFile by
0.006. The remainder was needed to remove the caveat, not to find the answer.

### Per repository, and a guess that did not survive

An earlier version of this note guessed that the subset understated the tool,
because the cheap-to-index repositories it favoured are small while the best
results seen so far were on large corpora. **Django is now complete — all 209
instances, the largest Python corpus in the set at ~22k symbols a snapshot —
and the guess did not hold.**

| Repository | Language | n | HitFile | HitRegion | Prec |
|---|---|---:|---:|---:|---:|
| scikit-learn | Python | 31 | 0.651 | **0.568** | **0.556** |
| matplotlib | Python | 30 | 0.640 | 0.532 | 0.444 |
| xarray | Python | 18 | 0.625 | 0.449 | 0.380 |
| **django** | Python | **209** | 0.610 | 0.436 | 0.343 |
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
size is not the driver — and django is the best-measured repository in the set
at 209 instances, so this is no longer a small-sample judgement: the first 50
gave 0.600, the next 91 gave 0.623 and the last 68 brought the whole to 0.610,
which is one repository measured three times rather than three pictures. It is
the strongest single repository by weight (0.610 against 0.502 for the other
639 instances combined), which is why finishing it raised the overall HitFile —
but the repositories above it are two orders of magnitude smaller. Language is the stronger signal: the top of the table
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

This sweep ran on a subset, so its 20-row (0.522) is not the full-set 0.529.
It only walks upward; the downward point that matters to a user — the served
default of 5 — is measured on all 848 at the top of this document.

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

**That experiment is contaminated and its conclusion does not stand.** Comparing
the two indexes file by file afterwards: on **21 of the 30 snapshots the corpora
are identical** — `--include-tests` added not one file. The filter is by
design a filename rule and not a path rule (`internal/index/walker.go` says so,
because directories named `test/` also hold non-test code), so a suite organised
as `test/topics.js` is indexed either way and the flag is a no-op for it. Twenty
of the thirty snapshots are NodeBB, whose tests are exactly that shape. The
design could therefore not show a difference on 21 of its 30 instances, which is
why 28 came back "unchanged"; the real sample was 9, of which 2 improved. Where
the flag does bite it bites hard — ansible +263 files, caddy +57 to +68, axios
+41 — and on carbon the one file it adds is `src/Carbon/Traits/Test.php`,
production code the `Test.php` suffix rule catches by mistake.

Re-run properly. Scanning all 689 indexed snapshots for files the flag would
add shows the corpus moves on 606 of them, so the old sample was close to the
worst one available. The replacement was chosen from that scan: 124 snapshots
across seven repositories and three languages — qutebrowser 34, vuls 21,
pytest 18, xarray 18, flipt 14, preact 11, requests 8 — every one of which
gains files (11,033 in total, 88 a snapshot, verified by comparing the two
corpora rather than by trusting the scan), and 25 of which have no test file in
their gold at all, because a sample of only the instances that can benefit
measures only benefit. Same protocol, `-max 40 -rerank 40`, both arms driven
identically:

| | default index | with tests | paired delta |
|---|---:|---:|---|
| File recall | 0.572 | **0.692** | **+0.121, t = 6.48** (61 better / 13 worse) |
| Prec | 0.271 | 0.221 | **-0.050, t = -4.09** (44 / 73) |
| Line recall | 0.050 | 0.040 | -0.010, t = -4.70 |
| F1 | 0.073 | 0.058 | -0.015, t = -4.76 |
| HitRegion | 0.376 | 0.392 | +0.016, t = 1.35 (not significant) |

**This is the largest reach effect in the campaign and it is not free.** File
recall rises 21% relative; precision falls 18% relative, line recall and F1 fall
with it, and the mean rank of the first useful result moves 3.05 -> 3.55.

Splitting by whether the instance's gold contains a test file settles what is
happening:

| | n | File recall | |
|---|---:|---|---|
| gold contains a test | 99 | **+0.171, t = 8.96** | 60 better / 5 worse |
| gold contains no test | 25 | **-0.077, t = -2.58** | 1 better / 8 worse |

Where the answer lives in a test file, indexing tests is worth a great deal.
Where it does not, test code crowds out implementation and the answer gets
worse — the cost this experiment was supposed to look for, found at last on a
sample able to show it, spread across five of the seven repositories (xarray
-0.155, pytest -0.200, flipt -0.067, vuls -0.028) rather than coming from one.

An earlier reading of this result on nine snapshots reported that precision rose.
At n = 124 it does not; that reading was noise.

**So this is a trade, not an upgrade, and it should not become the default on
the strength of a benchmark whose gold is "what a solver read" — solvers read
tests, and 80% of this sample's instances have a test in their gold, which is
not a property of agent work in general.**

**Tests in the index, out of the ranking — measured, and it does not do what it
was built to do.** `internal/retrieve/testfloor.go` reserves the first N answer
slots for non-test files; tests keep their relative order behind them and fill
what implementation leaves empty. Swept on the same 124 snapshots, against the
default (no-tests) index:

| | File recall | Prec | F1 | line recall |
|---|---:|---:|---:|---:|
| default index | 0.572 | 0.271 | 0.073 | 0.050 |
| tests, floor 0 | 0.692 | 0.221 | 0.058 | 0.040 |
| **tests, floor 10** | **0.692** | **0.244** | 0.065 | 0.045 |
| tests, floor 40 | 0.651 | 0.242 | 0.065 | 0.046 |

Floor 0 reproduced the earlier arm to three decimals on every metric, so the
sweep is measuring the knob and not the build.

**Floor 10 is free precision: +0.023 (t = 4.11, 38 better / 11 worse) for a file
recall delta of exactly 0.0000 — zero instances better, zero worse.** Half the
precision cost of indexing tests is recovered without giving up any of the reach.

Shipped on by default as a quarter of the answer rather than the measured count
of 10, since the served `max_results` is not the probe's. Checked at the second
point before the default stood: at `max_results` 20 (floor 5) precision goes
0.279 -> 0.313 (t = 4.07, 29 better / 4 worse), F1 0.065 -> 0.072, and file
recall again moves by exactly 0.0000 on all 124 instances. The effect is larger
at the smaller answer, which is what a budget argument predicts.

**It does not fix the instances that got worse, and cannot.** The 25 instances
whose gold holds no test file are still at -0.077, unchanged to four decimals.
Reordering a list cannot change which files are in it, and file recall is
membership. The harm is not tests outranking implementation; it is test symbols
taking slots in the returned 40 and pushing implementation files out of the list
altogether. The prediction that a floor would address it was wrong, and wrong
structurally rather than by a margin.

Only floor 40 changes membership — it pulls non-tests up from deeper to fill the
reserved slots — and it pays for it: the harmed group improves to -0.044
(no longer significant, t = -1.48) while the 99 that benefit fall from +0.171 to
+0.111, and overall file recall drops 0.692 -> 0.651 (27 instances worse, 7
better). Trading a third of the gain to remove two fifths of the harm is a bad
deal when the gaining group is four times larger — on this benchmark. On a
workload where the answer rarely lives in a test, the same arithmetic points the
other way, which is the whole reason not to fix the operating point here.

The honest statement is also narrower than the old one: what is excluded is
name-conventioned tests, not test code in general. The rest of the ceiling
decomposition above counts test files by path and is unaffected.

**The literal channel — built, fires, and does not help.**
`internal/retrieve/literal.go` scores files by how many of the query's
identifiers occur in them, weighted by each identifier's rarity in the
repository, and hands the last few answer slots to the best files the ranking
did not reach. On the same nine snapshots, on top of the corpus that contains
the tests, it moves file recall 0.643 -> 0.615: **better on zero instances,
worse on one.** A debug response confirms it emits exactly the requested
`literal_match` results in real files, so the null is the idea's and not the
wiring's. It ships off (`literal_slots`, `-literal`).

That is the third time on this benchmark that a candidate-side improvement
measured well as a set and returned nothing as an answer.

**Merging grep into the ranking — a ceiling that does not convert either.**
Literal identifier search has the property the graph lacks: a substring either
matches or it does not. Extracting the code identifiers from each issue the way
`-query-mode ids` does and scanning the snapshot for them, over 150 instances
and 471 gold files:

| files must contain | grep finds | union with ours | grep only | files per query |
|---|---:|---:|---:|---:|
| ≥1 identifier | 69.0% | 80.3% | 24.4% | 192 |
| ≥2 identifiers | 41.6% | 70.3% | 14.4% | 57 |
| ≥3 identifiers | 27.8% | 64.5% | 8.7% | 16 |

Against our own 55.8%, the union looks decisive. It is not, for two reasons
that only appear when the number is taken apart.

Of the 68 gold files grep alone finds at the ≥2 threshold, **52 are not in the
index at all, and 52 of those 68 are test files** — so three quarters of the
apparent advantage is the test-file filter measured in the row above, which
already failed to convert.

The other quarter is real, and whether it is worth having depends entirely on
how deep the answer goes. Replacing the tail of our answer with the best grep
candidates, ranked by identifier rarity within the repository, at a fixed number
of unique files:

| swapped | ours 10 files (max_results 20) | ours 18 files (max_results 40, rerank pool 40) |
|---|---:|---:|
| none | 0.558 | 0.596 |
| 2 | 0.565 (25 better / 19 worse) | **0.643** (27 / 6, sign test p = 0.0003) |
| 3 | 0.550 (27 / 27) | **0.647** (33 / 11, p = 0.0013) |
| 5 | 0.484 (22 / 40) | 0.639 (37 / 17, p = 0.009) |

**A grep candidate beats our own result from about rank 11 down.** At ten files
the swap discards results that were still discriminating and the trade is a
wash; at eighteen it replaces a tail that has stopped discriminating, and it is
the first significant reach improvement in this campaign. Two caveats bound the
claim: the simulation scores unique files with no line budget at all, where
HitFile scores regions under B=500, and a grep candidate arrives as a file with
no region, so it would spend that budget differently. It gives the sign of the
trade, not a HitFile prediction.

**Every other attempt in this section measured reachability and expected the
answer to follow.** It never has. The consequence shipped so far is not a
ranking change at all: an agent that has already run grep is holding a position,
and until
`internal/retrieve/locator.go` there was no way to ask this tool about one —
`find_context` took prose and `expand_context` took a `request_id` from a prior
`find_context`. A query of the form `path:line` is now answered by lookup,
returning the enclosing symbol with its callers and callees, with no embedding
or reranking on the path. That is composition rather than substitution: grep
says where a name occurs, the graph says what reaches it, and the measurement
above says competing with grep on the first question is not worth doing.

## The two retrieval gaps, taken apart

The decomposition above leaves two gaps of similar size — 13.1% of gold files in
the response below rank 20, and 14.3% never retrieved at all — and treats them
as one "retrieval problem". They are not the same problem and neither behaves as
expected.

**Reranking deeper makes the answer worse.** The ablation that reported "the
cross-encoder buys no reach" ran with `rerank_k` 15 against `max_results` 20: it
could not promote a candidate at rank 50 because it never saw one. Giving it the
pool, over 300 instances at a fixed answer size:

| rerank_k | HitFile | Prec | paired against 20 |
|---:|---:|---:|---|
| **20** | **0.556** | **0.346** | — |
| 100 | 0.475 | 0.302 | **-0.081, t = -6.84** (20 better / 93 worse) |
| 200 | 0.455 | 0.281 | **-0.101, t = -7.77** (25 better / 111 worse) |

So the narrow window is not the cross-encoder's handicap, it is its protection.
Handed two hundred candidates it promotes what looks superficially like 850
characters of issue prose and demotes what the fusion — vector, FTS and PageRank
together — had ranked correctly. **On this query shape the fusion is the better
ranker and the reranker is a local polish**, which is the opposite of what the
"+0.036 precision" line suggests in isolation. The 13.1% is therefore not a
cheap ranking win: the files are reachable and our ranker cannot pick them out.

**The never-retrieved 14.3% is mostly vocabulary, but not entirely.**
[`cmd/reachprobe`](../../cmd/reachprobe) asks, for each gold file the index
holds, whether the query's code identifiers appear in it — and reports the same
statistic over the files we DID retrieve, because the missed figure means
nothing alone:

| | files | identifiers in the file | in the text we index | on disk only |
|---|---:|---:|---:|---:|
| retrieved (control) | 431 | 77.0% | 75.9% | 1.2% |
| never retrieved | 52 | 40.4% | 30.8% | 9.6% |

Roughly 60% of the missed files share no vocabulary with the query at all — only
a better representation reaches those. About 31% have the words **in the indexed
text** and are still not among two hundred results, which is a lexical problem
and the one cheap lever left standing. The remaining 10% have the words on disk
but outside what we index, eight times the control rate, which points back at
the body cap rather than at any model.

The control line carries its own answer to "is the embedder too weak": for the
files we do retrieve the identifiers are present only 77% of the time, so a
quarter of our hits are semantic matches with no lexical overlap at all. The
embedder is working; it is not omnipotent on a query that describes a symptom
against code that implements a mechanism.

n = 52 on the missed row, so the shares carry about ±7 points.

**The lexical lever was built, and it fails for the same reason as the other
two.** Asked of FTS directly, an identifier-only query puts 75% of those missed
files inside the top fifty — median rank 34, against median rank 3 for the files
we do find — so the channel can see them and only the way we ask cannot. A fifth
seed channel searching the identifiers alone, at its own depth of fifty, was
added behind a weight and measured over 300 instances with a stopping rule fixed
in advance:

| | delta | t | better / worse |
|---|---:|---:|---|
| File recall | **-0.0135** | **-2.11** | 15 / 28 |
| Prec | -0.0040 | -0.88 | 100 / 90 |
| HitRegion | -0.0034 | -0.75 | 10 / 13 |

The first reading was displacement: the seed pool holds twenty, and a fifth
channel's votes push out candidates the other four had right. That reading is
wrong, and the test that settles it had never been run — the "pool 20 -> 200
changes nothing" sweep predates the channel, so the two had only ever been
measured apart. At a pool of sixty:

| | delta | t | better / worse |
|---|---:|---:|---|
| channel at pool 20 | -0.0135 | -2.11 | 15 / 28 |
| channel at pool 60 | -0.0096 | -2.07 | 11 / 23 |
| the pool alone, 60 vs 20, no channel | -0.0032 | -0.45 | 33 / 34 |

Tripling the room leaves the shape untouched, and the pool on its own is neutral
— so nothing was being crowded out and the personalization vector was not
diluted either. **The added signal is simply worse than what it replaces.**

The channel's own measurement says why. The wanted file sits at FTS rank 34,
which means an OR over eight identifiers ranks *thirty-three wrong files above
it*, and each of those votes in the fusion as well. The channel is precise about
the file we want and noisy about everything else, and RRF cannot tell the two
apart. Requiring two identifiers to agree before a file counts is the same idea
one step further, and that was measured separately as the literal channel: also
negative.

**This is the third appearance of one mechanism, and now it has an explanation
rather than three separate nulls.** Merging grep candidates into the answer,
reranking a deeper pool, and adding an identifier seed channel all add a source
that looks precise in isolation and is worse in aggregate than the existing
combination of vector, prose and graph. On an 850-character issue report the
fusion we already have beats any single signal we can add to it locally — which
is also why the seven earlier attempts returned nothing.

## Where a region miss actually happens

HitRegion divided by HitFile is 0.71 for us, 1.00 for CoSIL, 0.80 for Claude
Code — and 0.82 for BM25. Having found the file, we point at the right code less
reliably than a lexical baseline. That ratio is the sharpest statement of the
weakness in these tables, and it is stable per repository (0.57 on openlibrary
and teleport, 0.72 on django over 209 instances, 0.87 at best on scikit-learn).

Read as "we pick the wrong symbol", it suggests using the call graph to pick a
better one. [`cmd/regionprobe`](../../cmd/regionprobe) was written to test that
and reports something else. Over all 848 instances and 3992 gold regions:

| | regions | share |
|---|---:|---:|
| hit | 1205 | 30.2% |
| **file never found** | **2119** | **53.1%** |
| trimmed: right symbol, wrong part shown | 271 | 6.8% |
| wrong symbol | 247 | 6.2% |
| — one call-graph hop from an answer | 86 | 2.2% |
| — two hops | 55 | 1.4% |
| — further or unreachable | 106 | 2.7% |
| lines not in the index | 150 | 3.8% |

(Pooled over regions and without the line budget, so these are not the
per-instance HitRegion above; the decomposition is the point, not the level.)

**The graph hypothesis is nearly dead**: 3.6 points if two hops were used
perfectly. **Widening the evidence window is dead**: only 21% of trimmed misses
sit within ten lines of what was shown, the median is 42 and the p75 is 172.

What the numbers do say is that we return containers. In a trimmed miss the
returned symbol has a median length of 296 lines while the symbol actually
holding the gold has a median of 46, and in 64% of them that finer symbol was in
the index. We rank a class above its own method and then show 8% of the class —
which also explains why the same symbols are the ones the 2000-byte body cap
truncates.

The obvious remedy does not work. `internal/retrieve/nesting.go` prefers a
contained candidate over its container; it is written, tested, and deliberately
not wired in, because of the 36 misses where a finer symbol existed it was
itself returned in 4. At a hundred results that share rises from 11% to 41%, so
the member is in the candidate pool and merely ranks far below the container —
a promotion rule would have about 1.4 points to work with, paid for in the
precision that the max_results sweep already priced.

**So within-file work is capped at a few points and the mass is elsewhere.**
Everything inside the file — graph selection, window width, nesting, the
unindexed lines — adds to under 11 points against the 53.1% of gold whose file
we never reach at all.

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
