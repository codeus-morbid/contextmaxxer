package cli

// Guards the config-parity contract: the release config constants must match
// the packages they mirror, and served-path defaults must not silently drift
// from the measured release configuration again (they did, four ways, before
// internal/releasecfg existed).

import (
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/releasecfg"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

func TestReleaseConfigParity(t *testing.T) {
	if releasecfg.Reranker != rerank.JinaRerankerV1TinyEN {
		t.Fatalf("releasecfg.Reranker %q != rerank.JinaRerankerV1TinyEN %q — update releasecfg (it avoids the cgo dep on purpose)",
			releasecfg.Reranker, rerank.JinaRerankerV1TinyEN)
	}
	if !releasecfg.AdaptiveRerank {
		t.Fatal("adaptive rerank is part of the measured release config; turning it off requires a new gen-eval campaign")
	}
	if releasecfg.RerankK != 15 {
		t.Fatalf("rerank pool = %d; every published gen-eval number used 15 — changing it requires a new gen-eval campaign", releasecfg.RerankK)
	}
	// The weak-match floor lives on the reranker's own sigmoid scale, so it is
	// only valid for the reranker it was calibrated against.
	if releasecfg.Reranker != "jina-reranker-v1-tiny-en" || retrieve.WeakMatchRelevance != 0.70 {
		t.Fatalf("weak-match floor %.2f was calibrated for jina-reranker-v1-tiny-en (current: %q) — rerun cmd/confcal before changing either",
			retrieve.WeakMatchRelevance, releasecfg.Reranker)
	}
	if releasecfg.MaxResults != 5 {
		t.Fatalf("served max_results = %d; 5 was set with a measured hit/token tradeoff (see releasecfg DECISION) — changing it requires re-measuring", releasecfg.MaxResults)
	}
}
