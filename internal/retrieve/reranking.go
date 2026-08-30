package retrieve

import (
	"context"
	"time"
)

// rerankAndRank applies the cross-encoder, the intent ranker and the optional
// escalation pass, in that order, and returns the reordered candidates.
//
// Extracted from runPipeline verbatim — moved, not rewritten, so the 20-repo
// gate could prove the split changed nothing. The DECISION comments inside
// record why each stage is conditional.
func rerankAndRank(ctx context.Context, r *Retriever, req Request, scored []ScoredResult, stats *Stats) []ScoredResult {
	if !req.SkipRerank && r.reranker != nil && len(scored) > 1 && !shouldSkipRerank(req, scored) {
		// DECISION(2026-06): lazy mode inverts the default — the cross-encoder
		// (~1s, 83% of total latency after the vector cache) runs only when the
		// fused ranking is ambiguous. Confident fused rankings are served as-is.
		if req.LazyRerank && !rankingAmbiguous(scored) {
			stats.RerankLazySkipped = true
		} else {
			t3 := time.Now()
			reranked, err := r.reranker.Rerank(ctx, req.Query, scored)
			if err != nil {
				// A reranker failure (seen live: transient DML 80004005 on
				// GPU) must degrade to the fused ranking, not kill the whole
				// query — the fusion order is already a good answer.
				r.log.Warn("rerank failed; serving fused ranking", "err", err)
			} else {
				scored = reranked
			}
			stats.RerankDuration = time.Since(t3)
		}
	}
	applyIntent := func() {
		if r.ranker == nil || len(scored) <= 1 {
			return
		}
		if req.SkipIntent {
			if _, ok := r.ranker.(IntentRanker); ok {
				return
			}
		}
		tRank := time.Now()
		scored = r.ranker.Rank(req.Query, scored)
		if _, ok := r.ranker.(IntentRanker); ok {
			stats.IntentDuration += time.Since(tRank)
		}
	}
	applyIntent()

	// DECISION(2026-06): escalation — when the primary (fast) ranking looks
	// ambiguous, rerun the candidate pool through a stronger reranker. On the
	// gen corpus jina-v2 beats tiny by +0.08 Hit@1 but costs ~5.7s p95; paying
	// that only on low-confidence queries keeps the common path fast.
	// REVISIT IF: escalation rate exceeds ~40% of queries or quality matches tiny.
	if r.escalator != nil && !req.SkipRerank && len(scored) > 1 && rankingAmbiguous(scored) {
		tEsc := time.Now()
		esc, escErr := r.escalator.Rerank(ctx, req.Query, scored)
		if escErr != nil {
			r.log.Warn("escalation rerank failed", "err", escErr)
		} else {
			scored = esc
			stats.Escalated = true
			applyIntent()
		}
		stats.EscalateDuration = time.Since(tEsc)
	}

	return scored
}
