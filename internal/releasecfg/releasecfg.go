// Package releasecfg is the single source of truth for the measured release
// configuration — the exact settings every published gen-eval number was
// produced with ("tiny cross-encoder + intent ranker + adaptive rerank").
//
// DECISION(2026-07): this package exists because the served paths (mcp,
// query) silently diverged from the eval config FOUR separate ways (intent
// ranker not wired, rerank pool 8 vs 15, adaptive off, paraphrastic protect
// fix disabled) and no benchmark can catch that class of bug — benchmarks
// exercise the eval path. Flag defaults and retriever construction in cli
// and cmd/eval must reference this package instead of restating values.
// REVISIT: whenever a new gen-eval campaign promotes a different config,
// change it HERE and only here.
package releasecfg

import (
	"log/slog"

	"github.com/codeus-morbid/contextmaxxer/internal/intent"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

const (
	// Reranker is the default cross-encoder. Must equal
	// rerank.JinaRerankerV1TinyEN (asserted by a test in internal/cli; not
	// imported here to keep this package free of the cgo tokenizer dep).
	Reranker = "jina-reranker-v1-tiny-en"
	// RerankK is the candidate-pool size the cross-encoder rescores.
	RerankK = 15
	// AdaptiveRerank skips the cross-encoder for confident exact/constructor
	// top matches.
	AdaptiveRerank = true
	// FullBodies is the tiered-packing default: top results with full bodies,
	// the rest compact.
	FullBodies = 3
	// MaxResults is the served default result count.
	// DECISION(2026-07): 8 -> 5 after the intent-ranker campaign lifted
	// R@5 to 0.91 (dev n=86; holdout flat at 0.91). Measured cost: 3/144
	// gen cases have their only hit at rank 6-8 (~2pp of visible hits) for
	// ~60-100 tokens saved per call. ASSUMES: agents rephrase-and-retry on
	// a miss (hook/rule instructs this). REVISIT IF: beta feedback shows
	// miss-then-giveup instead of miss-then-retry.
	MaxResults = 5
)

// NewRetriever builds the retriever exactly as the release config was
// measured: with the intent ranker installed. Serving paths must use this
// instead of the raw retrieve constructors.
func NewRetriever(st retrieve.Store, e retrieve.Embedder, rr retrieve.Reranker, log *slog.Logger) *retrieve.Retriever {
	return retrieve.NewRetrieverWithRankers(st, e, rr, intent.NewRanker(), log)
}
