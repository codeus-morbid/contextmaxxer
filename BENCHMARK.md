# Product benchmark and research log

The agent-level comparison in the README is measured on a **public** repo so you
can reproduce it yourself — no trust required. This uses Prometheus; the same
recipe works on any large Go codebase.

## How to read this document

- **Product benchmark** covers the agent-level Prometheus, CockroachDB, Django
  and NestJS comparisons. The repositories and retrieval commands are public;
  the full paired-agent table also requires an MCP host and agent harness.
- **Research appendix** covers CORE-Bench embedder and training experiments.
  It is evidence about model and fusion design, not the shipped product path.
- **Archived negative results** records approaches that were tested and parked.

Only results whose required artifacts are public should be described as fully
reproducible. The `ft2` weights are published with a model card
([jina-v2-code-ft2](https://huggingface.co/codeusmorbid/jina-v2-code-ft2),
CC BY-NC-SA 4.0); the training pipeline and its data are not, so those rows stay
recorded research results rather than release claims.

## Setup

```bash
git clone https://github.com/prometheus/prometheus
cd prometheus && git checkout 2ad3a87   # pin the exact commit we measured
# index it (GPU is picked automatically; CONTEXTMAXXER_ORT_PROVIDER=cpu|cuda|directml forces one)
contextmaxxer index .
# -> Indexed ~535 files, ~9391 symbols
```

## The questions

Five deliberately *paraphrastic* discovery questions — phrased by intent, not by
the identifiers in the code, so plain `grep` has to guess keywords:

| # | Question (intent) | Answer lives in |
|---|---|---|
| 1 | mark a series stale when its scrape target disappears | `scrape/scrape.go` `updateStaleMarkers` |
| 2 | flush in-memory head samples into immutable on-disk blocks | `tsdb/db.go` `compactHead` |
| 3 | avoid scraping the same target twice across SD mechanisms | `scrape/scrape.go` `scrapePool.sync` (`Target.hash`) |
| 4 | cap how many samples one query may load (OOM guard) | `promql/engine.go` `evaluator` (`maxSamples` / `ErrTooManySamples`) |
| 5 | remote write not losing samples when the endpoint is down | `storage/remote/queue_manager.go` `sendSamplesWithBackoff` |

## Reproduce the retrieval (one command each)

```bash
contextmaxxer query --index ./.contextmaxxer/index.db \
  --reranker jina-reranker-v1-tiny-en "mark a time series as stale when its scrape target disappears"
```

The relevant symbol comes back in the top results with a line-numbered evidence
span — no file-opening needed. All 5 land in-tool (5/5) even without `enrich`.

## The agent comparison (what the README cites)

Two paired sonnet sub-agents answered the 5 questions: one restricted to
`grep`/`glob`/`read`, one driving `find_context`. Both got **5/5 correct**.

| Arm | Tool calls | File reads | Tokens | Wall-clock |
|-----|-----------|-----------|--------|-----------|
| grep-driven        | 51 | 4 | 50,840 | 142s |
| find_context (MCP) | **8** | **0** | 44,795 | **47s** |

**6.4× fewer tool calls, 3× faster, equal correctness.** Raw token cost is near
parity — the agent's own base context (system prompt + task, re-read every turn)
dominates total spend, so the win is round-trips, latency and context-window
headroom, not raw tokens.

> Reproducing the *retrieval* (the `query` commands above) is turnkey.
> Reproducing the full *agent* table needs an MCP host (Claude Code / Cursor) and
> a sub-agent harness; the numbers above are a single run, no cherry-picking.

**How much a single run is worth (2026-08).** Two runs of the *same* arm, same
prompt, same binary, same repo returned 52.1K and 81.8K variable tokens — a 57%
spread, wider than most of the between-repo differences on this page. The spread
comes from a discrete choice the model makes (how often it asks for a full body),
so it does not average away within a run. Read every ratio here as one sample:
the direction of the large effects (tool calls, source reads) is stable across
every run we have, but **differences below roughly 2× are not distinguishable
from run-to-run noise**, and no claim on this page about an effect growing with
codebase size survives that error bar. Deterministic probes were built for this
reason: `cmd/chainprobe` prices call-chain navigability and `cmd/rspbreak` prices
the response, both without an agent in the loop.

**Replication on CockroachDB (v0.1.0-beta.5, 2026-07):** same paired-sonnet
protocol, 6 deliberately paraphrastic questions (load-based splits, deadlock
victim choice, allocator scoring, GC TTL plumbing, SQL→AST, dead-node
up-replication) on the ~90K-symbol cockroach index. Both arms 6/6 correct;
find_context: **7 tool calls vs 34 (4.9×), 0 file reads vs 9, 45s vs 96s
wall**, agent tokens near parity (55K vs 48K). Honest caveat: cockroach is in
the model's training data, which props up the grep arm's correctness; the
structural win (round-trips, zero reads, latency) is what transfers to
unfamiliar code.

## Bigger task: end-to-end trace, and where the edge begins

Single discovery questions understate the tool — they're dominated by the agent's
fixed base context. On a *multi-step* task the discovery efficiency compounds. We
ran the same paired-Sonnet A/B on two public repos, each tracing one signal
end-to-end across packages, after improving call-graph completeness (type-aware
callee resolution). Whether the advantage also *scales* with codebase size is a
claim this page used to make and no longer does: the runs that would establish it
are single samples on a measurement whose repeat spread is 57%.

- **Prometheus** (~8.7k symbols): a sample's lifecycle — scrape → parse → append →
  WAL → head → compaction → query read-back.
- **CockroachDB** (~90k symbols, ~10× larger): a SELECT query — pgwire → optimizer
  → DistSQL → KV → Pebble → back.

Both arms covered every stage correctly. Numbers are harness telemetry (tokens,
wall-clock, total tool calls); *source reads* is the count of repo files opened.
Measured on the shipped binary (type-aware graph + false-edge fix + on-disk vec
cache).

| Repo | Arm | Tool calls | Source reads | Tokens | Wall-clock |
|------|-----|-----------|-------------|--------|-----------|
| Prometheus (8.7k) | grep-driven        | 77 | 13 | 86,551 | 276s |
| Prometheus (8.7k) | find_context (MCP) | **20** | **0** | **64,484** | **102s** |
| CockroachDB (90k) | grep-driven        | 98 | 28 | 109,890 | 489s |
| CockroachDB (90k) | find_context (MCP) | **16** | **0** | **61,972** | **112s** |
| Django (11k, Python) | grep-driven        | 42 | 12 | 58,932 | 175s |
| Django (11k, Python) | find_context (MCP) | **28** | **0** | **52,406** | **135s** |

**The edge is two-stage:**

- **Operations, source reads and latency win at every scale measured.** At ~9k
  symbols: 3.9× fewer tool calls, **0 file reads vs 13**, 2.7× faster. At 90k:
  6.1× fewer calls, **0 vs 28 reads**, 4.4× faster. The tool arm follows the call
  graph (`callers`/`callees` + `call_line`) instead of opening files, so it holds
  ~1 query per hop. **Zero source reads is the one result that reproduces in every
  run**; the ratios are single samples (see the variance note above), and the
  spread between 3.9× and 6.1× is not evidence that the advantage grows with size
  — that comparison sits inside the error bar.

- **Token totals are close, and the direction is not settled.** 1.34× on
  Prometheus, 1.77× on CockroachDB, and an earlier CockroachDB run at parity when
  the call graph was incomplete and the tool arm still read ~10 files. Three
  samples spanning 1.0× to 1.77× on a measurement whose repeat spread is 57% do
  not establish a trend either way. What is structural: the tool arm's spend does
  not climb with reads, because it does not read (0 vs 28).

- **It generalizes beyond Go.** Django (Python, 11k symbols): the tool arm still
  traced the whole request lifecycle (WSGI → middleware → routing → view → ORM →
  SQL → backend → response) with **0 file reads vs 12**, 1.5× fewer calls and 1.12×
  fewer tokens. The token margin is modest — Django is mid-size and unusually
  grep-friendly — but the **0-reads property holds across languages**. (This
  benchmark also surfaced and fixed a Python call-graph bug — the suffix resolver
  cross-linked every same-named method into a hairball; the numbers here are
  post-fix.)

- **TypeScript too — after two extractor fixes.** NestJS (TS, 2.7k symbols):
  tracing the request pipeline (adapter → router → guards → interceptors/pipes →
  controller → response), the tool arm hit **0 file reads vs 13**, covering every
  stage via caller/callee + `call_line`. Tokens and latency were ~parity here —
  NestJS is small (crossover zone) and DI-heavy — so the win is structural (full
  graph trace, no file opens), not yet economic. Getting there required fixing the
  TS extractor: exported-class methods produced no edges at all, and member calls
  (`this.svc.method()`) now resolve via the field's declared TS type. The call
  graph went from 24 edges to 2014 (0.74/symbol, the Go/Python range).

**The honest takeaway:** the robust, universal win is **round-trips, source reads
(→0), and latency** — large from ~9k symbols and widening with size. **Raw token
savings track size too** (≈1.3× at 9k → ≈1.8× at 90k), once grep's file reads on
deep code start to dominate. Position the tool on large codebases: that's where
every edge is largest. (Run-to-run numbers vary ~10–20% with model variance; the
direction and scaling are the stable signal.)

## Research appendix: embedder A/B on public data (CORE-Bench)

To ground the retrieval quality in a benchmark we don't control, we run the
embedding stage against a subset of **CORE-Bench** (arXiv:2606.11864 v3, EMNLP 2026, HF
`zhangfw123/CORE-Bench`) — Level-2 *issue-to-edit localization*: real GitHub
issues as queries, repo chunks as the corpus, per-query temporal filters.
Subset: 7 public repos (3 Python, 2 JS/TS, 2 Go), ~23K chunks, 156 queries.
Runner: `cmd/corebench` (measures the embedder inside our runtime — tokenizer,
pooling, prefixes, ONNX). Absolute scores are low by design — the paper itself
documents a "sharp drop" from classic code search to agentic retrieval — so
compare deltas, not absolutes.

**Reproduced 2026-08-03 on current code, exactly.** Per-repo, embed mode:
dayjs 0.3284/0.8480 (published 0.328/0.848), sqllineage 0.1089/0.4109
(published 0.109/0.411).

**Re-verified 2026-09-08 by recomputation, not from cache.** The GPU work since
(length-bucketed batching with fixed padding shelves) could in principle have
moved the vectors, so both repos were re-embedded from an empty cache: dayjs
0.3284/0.8480 and sqllineage 0.1089/0.4109 again, identical to four decimals.
The batching change is a throughput change and numerically inert. Reading the
same figures out of the warm cache would have proved nothing, since those
vectors predate the change. Nothing in the retrieval work since has moved the
embedding stage — and it cannot: CORE-Bench supplies its own corpus
(`{_id, text}` chunks with no paths, symbol names or kinds), so our
extractors and the symbolic intent ranker are not in this path at all. That
makes these numbers a clean isolation of the embedder, which is what a
model-vs-model comparison should measure.

Full Level-2 at scale (rerank mode, same embedder, 2026-08-03) — a different
measurement from the 7-repo subset above, not a comparison to it:

| sub-dataset | repos | queries | Pooled NDCG@10 / R@100 |
|---|---|---|---|
| SWE-bench-Live | 206 | 1592 | 0.1088 / 0.4531 |
| SWE-bench-plus | 6 | 207 | 0.1229 / 0.4715 |
| SWE-bench_Multilingual | 41 | 276 | **0.0557 / 0.3287** |

The multilingual split scores roughly half the others. The default encoder is
English-and-mainstream-trained, so breadth of language is where its headroom
is thinnest — worth stating whenever the "161M beats 7B" framing comes up.

Baseline, `jina-embeddings-v2-base-code` (the shipped default):

| Metric | Pooled (156 q) | Macro (7 repos) |
|---|---|---|
| NDCG@10 | **0.186** | 0.136 |
| Recall@100 | **0.510** | 0.402 |

Throughput on a consumer GPU (DirectML): **49 docs/s** indexing (after
length-bucketed batching with fixed padding shelves — was 27–35; large GPU
batches measured *slower* on DirectML, so the default batch stays 32),
51–76 ms per query embed. This corpus is long documents; symbol corpora
(short, mixed-length texts) benefit more from bucketing.

**Candidate tried and rejected: Qwen3-Embedding-0.6B (int8 ONNX).** The obvious
"newer, bigger" upgrade (Apache-2.0; the stronger jina-code-embeddings is
CC-BY-NC and unshippable). Integration verified healthy with `cmd/embprobe`
(clear semantic separation), yet on both repos measured it **lost to the
5-year-smaller default on quality** — sqllineage 0.095/0.308 vs 0.109/0.411,
dayjs 0.293/0.832 vs 0.328/0.848 (NDCG@10/R@100) — while indexing **12–28×
slower** (1–2.2 docs/s) with 300 ms query embeds on GPU. int8 quantization
and untuned instruction wording may account for part of the quality gap, but
the latency alone disqualifies the 0.6B-decoder class for a local instant-on
tool. The model spec was removed from the binary after the verdict; the
evaluation kit (`cmd/corebench`, `cmd/embprobe`, and last-token-pooling /
instruction-prefix support in the embedder) remains for future candidates.

**Takeaway:** at local-inference budgets, the small code-trained encoder is
still the right default; quality upgrades should come from the graph and
enrichment layers (and possibly a future code-specific small encoder), not
from scaling the embedder.

### Beyond the raw embedder: the seed stage on CORE-Bench

The production pipeline doesn't rank with the embedder alone — it RRF-fuses
vector and keyword (FTS) seeds, then lets PageRank/intent/rerank stages
reorder. `corebench -mode hybrid` reproduces that seed fusion (BM25 + RRF,
same k=60) on the same subset:

| Stage | NDCG@10 | Recall@100 |
|---|---|---|
| vector only | **0.186** | 0.510 |
| vector + BM25 RRF (production seeding) | 0.163 | **0.631** |
| + cross-encoder on top-50 | 0.102 | 0.631 |

Two honest findings. **Hybrid seeding buys +24% recall** (0.510 → 0.631) —
and recall is the seed-stage KPI, because downstream stages re-rank whatever
the seeds catch. The top-10 order degrades slightly under fusion on this
small subset (long issue texts make BM25 noisy at rank 1-10).
**The tiny cross-encoder does not transfer to issue-to-edit chunks** (0.163 →
0.102): it rescores raw 2 KB code chunks against multi-KB issue reports,
while in production it scores compact symbol cards (signature + docstring +
excerpt) against short developer queries — where a held-out eval measured it
at +0.36 Hit@1. Scope noted; no product change.

### The full Level-2 set

We then ran three of the four Level-2 sub-datasets in full — Multi-SWE-bench,
SWE-bench-Live and SWE-bench_Multilingual: **253 repos, 2,075 scoreable
queries, ~2.2M corpus chunks** (SWE-Bench-plus-plus excluded: 5.4M chunks for
only 442 queries; it is reserved as uncontaminated fine-tuning data instead).
Overnight on one consumer GPU; per-repo embedding throughput 42–73 docs/s.

| Full L2 (2,075 q) | NDCG@10 | Recall@100 |
|---|---|---|
| vector only | 0.122 | 0.383 |
| vector + BM25 RRF (production seeding) | **0.150** | **0.438** |
| *paper, full set:* SweRankEmbed-Large | 0.224 | 0.521 |
| *paper, full set:* Qwen3-Embedding-8B (no fine-tune) | 0.203 | 0.480 |
| *paper, full set:* gte-Qwen2-1.5B / bge-m3 | 0.035 / 0.046 | 0.159 / 0.183 |

On the full set hybrid fusion improves BOTH metrics (on the small subset it
traded top-10 order for recall) — with 20-30K-chunk corpora the BM25 signal
helps precision too. Position honestly: a 161M local encoder + BM25 sits
well above general-purpose embedders and within reach of the specialized
SweRankEmbed-Large, below fine-tuned 8B models.

### The fine-tune: a 161M local model past SweRankEmbed-Large

The paper's own headline — supervised fine-tuning lifts NDCG@10 by ~60%
relative — reproduced on consumer hardware. `cmd/ftdata` extracted 1,604
(query, positive, hard-negatives) triplets from the three sub-datasets with
a **repo-level train/holdout split** (206 train repos; 47 held out, including
every repo in our published subset; `split.json` is the contract). Training:
sentence-transformers MNRL with 4 BM25-mined hard negatives per row, bf16,
3 epochs — **3 hours on one RTX 3060**. Evaluated only on the 47 holdout
repos (468 queries) the model never saw:

| Holdout (468 q) | NDCG@10 | Recall@100 |
|---|---|---|
| jina-v2-base-code, vector | 0.142 | 0.420 |
| jina-v2-base-code, hybrid | 0.168 | 0.491 |
| **fine-tuned, vector** | 0.141 | 0.456 |
| **fine-tuned, hybrid** | **0.262** | **0.561** |

**+56% relative NDCG@10 on hybrid**, past SweRankEmbed-Large's published
full-set 0.224 (different query sets — indicative, not head-to-head). The
mechanism is instructive: vector-only NDCG barely moves, but the fine-tuned
model surfaces *different* relevant chunks than BM25, so the RRF fusion
compounds. Guard check on production-style short developer queries (30-case
gen corpus, unenriched indexes both sides): no regression (Hit@1 0.53 vs
0.50, within noise) — the issue-tuned model did not forget short-query
retrieval. Local model name: `jina-v2-code-ft` (training pipeline under
`_bench/corebench-full/ft/`).

**Productization gate (full 144-case gen-eval, enriched indexes, 2026-07):**
the 30-case pilot's "no regression" did NOT replicate at full size. On the
86-case dev split ft2 hybrid trails the base embedder (Hit@3 0.79 vs 0.85,
R@5 0.84 vs 0.88, spread across projects and slices); the 58-case holdout is
at parity (Hit@3 0.90 both, R@5 0.93 vs 0.91). Issue-tuned wins do not
transfer to short developer queries, so **the product default stays
jina-v2-base-code**. The fine-tuned models are local research artifacts, not
distributed release options; `jina-v2-code-ft2` is retained only as the model
identifier used by the evaluation tooling.

**Round 2 — the contamination-free full-set number.** To remove even the
holdout-selection caveat, a second model (`jina-v2-code-ft2`) was trained
ONLY on SWE-Bench-plus-plus — the fourth Level-2 sub-dataset, with zero
repository overlap with the evaluation set — and evaluated on the entire
evaluation set (same repos as the baseline table above, ~2,080 queries):

| Full set, same repos (~2,080 q) | Params | NDCG@10 | Recall@100 |
|---|---|---|---|
| *paper:* gte-Qwen2-1.5B (general-purpose) | 1.5B | 0.035 | 0.159 |
| *paper:* bge-m3 (general-purpose) | 568M | 0.046 | 0.183 |
| *paper:* CodeRankEmbed (code-specific) | <1B | 0.121 | 0.329 |
| jina-v2-base-code, hybrid (our baseline) | 161M | 0.150 | 0.438 |
| *paper:* Qwen3-Embedding-8B, zero-shot | 8B | 0.203 | 0.480 |
| *paper:* SweRankEmbed-Large (CC-BY-NC) | 7B | 0.224 | 0.521 |
| **ft2 (external training data), hybrid** | **161M** | **0.233** | **0.498** |
| *paper:* Qwen3-8B-SFT (their fine-tune) | 8B | 0.328 | 0.664 |

**+55% relative NDCG@10, no shared repositories between training and
evaluation — and past SweRankEmbed-Large's NDCG@10 with a 161M model** that
runs locally and ships under a commercial-friendly license. Read the table
both ways: 6.6× above general-purpose embedders (gte-Qwen2-1.5B), 1.9× the
paper's own sub-1B code-specific model (CodeRankEmbed, 0.121 — the closest
comparison by size, which the unmodified default already passes at 0.150), and
ahead of an 8B zero-shot and the 7B specialized retriever on NDCG@10 — but
recall stays below SweRankEmbed-Large's (it does clear the 8B zero-shot's
0.480), and the paper's own fine-tuned 8B
(Qwen3-8B-SFT, 0.328) remains clearly ahead of everything local-sized. A
locally-run Qwen3-Embedding-0.6B was also tried as a base and rejected
(worse quality, 12–28× slower to index; see the earlier section). The two
independent fine-tunes on disjoint training sets (+56% holdout, +55% full)
replicate the same relative gain, and both show the same mechanism
(vector-only nearly flat; the gain lives in fusion).
Remaining set-difference caveat: our evaluation excludes Multi-SWE-bench
(absent from the baseline run) and plus-plus (training data); theirs is the
full original set.

### Archived negative results

Three follow-up attempts to push past ft2's 0.233, all evaluated against the
same bars and parked honestly:

| Experiment | Result | Verdict |
|---|---|---|
| ft3: ft2 recipe + paraphrased (Rewrite-L2) training queries | 0.271 vs ft2's 0.268 on a 7-repo/282-q screen | Noise (<2pp rule); full re-embed not justified |
| Fine-tuned cross-encoder rerank (jina-tiny, 2 recipes) | 0.078 vs hybrid's 0.241 on an 8-repo smoke | Learns the training pairs, not the task; parked after two attempts |
| Chunk-graph PPR blended into fusion (`-mode graph`) | 0.213 vs hybrid's 0.221, full 288-repo set (α=0.9 subset check: converges to hybrid from below) | No positive contribution at any blend weight |

The graph result is a statement about *this benchmark*, not about the graph:
CORE-Bench scores pinpoint edit localization, and PPR deliberately pulls in a
change's neighborhood — callers, callees, siblings — which is the part an
agent wants for context but the qrels don't credit. The production pipeline
(where the graph operates on real files with full call edges, not chunks
reconstructed from benchmark text) is unaffected; `-mode graph` stays in
`cmd/corebench` as the harness for future graph experiments.

The cross-encoder failure had one useful side effect: making the Go inference
path byte-faithful to training exposed that Roberta-tokenizer rerankers (the
production default `jina-reranker-v1-tiny-en` included) were being fed
BERT-style pairs — wrong separator layout and fabricated segment ids. Fixed
in `internal/rerank` (commit 86210c0); the fix is inert for the benchmark
numbers above (they don't use reranking) but makes the production reranker
see inputs in its native format for the first time.

## Try a bigger one

Point the same recipe at any large Go repo (CockroachDB, Moby, Terraform, Grafana):
the grep arm's round-trips and file reads climb with size while find_context stays
flat at ~1 query per step.
