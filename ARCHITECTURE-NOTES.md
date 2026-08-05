# Architecture notes — where Contextmaxxer sits, and why

Context for this note: the launch of **SubQ** (Subquadratic, May 2026) and its
*SSA — Subquadratic Sparse Attention* (12M-token context, content-routed top-k
attention) prompted the obvious question — *is what we do just an instance of the
same idea, and do we hit the same ceiling?* The short answer: **no, and the
reasons are useful to write down**, because they double as our differentiator.

There are two different "SSA"s in the literature, easy to confuse:
- **Subquadratic's SSA** — the product. A learned router scores all keys against
  each query token in O(n) and runs exact softmax over the selected k. Linear
  cost, content-aware routing, exact within budget.
- **arXiv:2511.20102 "Sparse Sparse Attention"** — a *training method*, not a
  model. It identifies why naive learned-sparse training underperforms and fixes
  it. This is the one with the transferable lesson for us.

## The taxonomy (Louis Wang's survey) and where we are NOT

The four subquadratic-attention families:
1. Fixed-pattern sparse (Longformer, BigBird) — routes by position.
2. **Learned sparse / content-routed top-k** (HiP, SeerAttention, DeepSeek NSA,
   SubQ's SSA) — a learned proxy scores keys, hard top-k, exact attention inside.
3. Linear attention (Performer, Linformer) — approximate softmax, O(n).
4. SSMs (Mamba, S4) — fixed-dimension recurrent state; compression bottleneck.

Contextmaxxer is **not an attention mechanism** — it's an offline retrieval
system (vector KNN + BM25 → RRF → Personalized PageRank over the call graph →
cross-encoder rerank → token-budget packing). The connection to family (2) is
*conceptual*, and it's exactly where the shared ceiling lives.

## The shared ceiling: one-shot similarity + hard top-k

A family-(2) router and a pure-vector retriever do the same thing: reduce
"relevance" to proximity in a learned space, then cut by a hard threshold. The
academic SSA result makes the limit precise:

> **Approximation error scales linearly with the attention mass dropped**, and
> **naive sparse training underperforms even when inference-time sparsity is
> optimal** — the gap is structural (the proxy is trained against the very thing
> it's trying to select), not a tuning problem.

For us, the proxy is the vector+reranker score and the hard cut is `max_results`.
The lesson: **pushing the embedder/reranker for the last few points of recall is
fighting a symptom of dropped mass, not the cause.** Same diminishing-returns
trap SubQ's family is in.

Their reported failure mode confirms it: **all-or-nothing** on 8-needle retrieval
(retrieves all 8 or fails entirely), *no graceful degradation*.

## What we have that family (2) does not

Two escapes from the one-shot-similarity ceiling are already in the architecture:

1. **A structural channel (Personalized PageRank over the call graph).** Code has
   exact relations text does not — callers/callees, def-use, imports. A symbol
   with weak cosine but high graph centrality still surfaces. This is the **graceful
   degradation** a hard router lacks by construction.

2. **Agentic multi-hop.** `find_context` is called iteratively by the agent:
   retrieve → read → re-query. Emergent relevance ("B matters only because A
   pointed at it") is discovered over hops, not in one pass.

The academic SSA fix — *align the sparse output to the full-attention output* —
has a direct analog here: **anchor the vector proxy to the structural ground
truth (the call graph).** Our hybrid + PPR *is* that alignment. The paper is, in
effect, a theoretical argument for why a pure-vector retriever needs a structural
anchor — which we already are.

## Our real bottleneck — two levers at two pipeline stages

The graph walk is **seeded by a vector hit**. If the seed misses (paraphrase),
the walk never starts. So the structural channel *amplifies* good seeds but can't
*rescue* a missed one. The two levers are complementary and sit at different
stages:

| Stage | Ceiling | Lever |
|---|---|---|
| Vector seed (recall) | similarity / paraphrase | late interaction (ColBERT MaxSim), query expansion |
| Graph completeness (amplify) | sparse / missing edges | type-aware callee resolution, import & def-use graphs |

- **Graph completeness** is the higher-leverage lever and has the stronger
  evidence: on CockroachDB, deep traces failed because the call graph dropped
  ambiguous method calls (`Send`/`Scan`/`Next`). Type-aware receiver resolution
  (commit bc3c1a2) densifies it. This is direction (1).
- **Late interaction** targets the seed stage. Offline experiment
  (`cmd/lateexp`, `EmbedTokens`): on contextmaxxer's own 436-symbol corpus
  (n=18 paraphrastic), MaxSim never regressed and rescued 1 case into the
  mid-ranks (+~5.6pp at recall@10/@20), but the corpus is too small for seed
  misses to be common — promising, not yet justified against the cost of
  per-token vector storage. Revisit on a large indexed repo.

## Differentiator for the beta

SubQ's own writeup admits the open gap: *no benchmark tests multi-hop reasoning
over 1M+ tokens where the answer must be synthesized from evidence that can't be
retrieved in isolation.* That is **precisely the regime where our agentic
multi-hop beats a one-shot router** — and it's a code-navigation task, where the
structural channel is exact. We should position on round-trips / context-window /
latency over a large codebase, and on multi-hop synthesis — not on raw token
savings (which the A/B shows is situational) and not in a recall-number race with
a frontier LLM (a race on the very metric that has a structural ceiling).

## Sources
- arXiv:2511.20102 — Sparse Sparse Attention (training method; attention/capability gap)
- Louis Wang — *A Survey of Subquadratic Sparse Attention*
- Subquadratic / SubQ launch materials (felloai, codiste reviews)
