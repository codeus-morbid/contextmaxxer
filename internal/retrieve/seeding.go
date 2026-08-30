package retrieve

import (
	"context"
	"fmt"
	"sort"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// seedOrigin records which channels produced a seed and its fused score.
type seedOrigin struct {
	score float32
	fromV bool
	fromF bool
}

// seedResult is what the seeding phase hands to the rest of the pipeline.
type seedResult struct {
	vSeeds       []store.ScoredSymbol
	seeds        []int64
	vectorScores map[int64]float32
	originMap    map[int64]*seedOrigin
}

// seedCandidates runs the seed channels and fuses them.
//
// Extracted from runPipeline verbatim — the body below is the same code that
// used to sit inline, moved rather than rewritten so the 20-repo gate could
// prove the split changed nothing. See the DECISION comments inside for why
// each channel is weighted the way it is.
func seedCandidates(ctx context.Context, r *Retriever, req Request, qvec []float32) (seedResult, error) {
	var err error
	var vSeeds []store.ScoredSymbol
	if qvec != nil {
		vSeeds, err = r.store.SearchByVectorScored(ctx, qvec, req.SeedK)
		if err != nil {
			return seedResult{}, fmt.Errorf("seed search: %w", err)
		}
	}

	var seeds []int64
	vectorScores := make(map[int64]float32)
	originMap := make(map[int64]*seedOrigin)

	if req.Mode == ModeVectorOnly {
		seeds = make([]int64, 0, len(vSeeds))
		for _, s := range vSeeds {
			seeds = append(seeds, s.ID)
			vectorScores[s.ID] = s.Score
			originMap[s.ID] = &seedOrigin{score: s.Score, fromV: true}
		}
	} else {
		fSeeds, err := r.store.SearchByText(ctx, req.Query, req.SeedK)
		if err != nil {
			return seedResult{}, fmt.Errorf("fts seed search: %w", err)
		}

		rrfScores := make(map[int64]float32)
		for rank, s := range vSeeds {
			rrfScores[s.ID] += 1.0 / float32(rrfK+rank+1)
			if _, ok := originMap[s.ID]; !ok {
				originMap[s.ID] = &seedOrigin{}
			}
			originMap[s.ID].fromV = true
			vectorScores[s.ID] = s.Score
		}
		// DECISION(2026-06): FTS gets a reduced vote in RRF. Equal-weight fusion
		// systematically demoted correct vector candidates on paraphrastic
		// queries (gen-corpus fb_g_encrypt_key: vector rank 3 -> hybrid rank 14;
		// ctx_g_alpha_balance: 13 -> out of pool). FTS still boosts identifier
		// matches, but cannot outvote strong semantic evidence alone.
		// REVISIT IF: lexical/identifier slices regress on the gen corpus.
		ftsW := DefaultFTSWeight
		for rank, s := range fSeeds {
			rrfScores[s.ID] += ftsW / float32(rrfK+rank+1)
			if _, ok := originMap[s.ID]; !ok {
				originMap[s.ID] = &seedOrigin{}
			}
			originMap[s.ID].fromF = true
		}
		// DECISION(2026-08): the lossless bodies are a third channel with their
		// own ranking. The head index stops at body_excerpt, so a term living
		// past the cap is unreachable through it — the only way such code gets
		// seeded at all. Weighted below head FTS because a long body matches
		// common tokens easily. REVISIT IF: lexical slices regress on the gate.
		bSeeds, err := r.store.SearchByBodyText(ctx, req.Query, req.SeedK)
		if err != nil {
			return seedResult{}, fmt.Errorf("body fts seed search: %w", err)
		}
		bodyW := DefaultBodyFTSWeight
		for rank, s := range bSeeds {
			rrfScores[s.ID] += bodyW / float32(rrfK+rank+1)
			if _, ok := originMap[s.ID]; !ok {
				originMap[s.ID] = &seedOrigin{}
			}
			originMap[s.ID].fromF = true
		}
		// DECISION(2026-08): chunk vectors are the semantic counterpart of the
		// body FTS channel. The per-symbol vector is built from the capped
		// excerpt, so a paraphrase of code living past the cap matches nothing
		// lexically and nothing semantically either; body FTS answers only the
		// first half of that. Chunks exist for capped symbols alone.
		if qvec != nil && DefaultChunkVecWeight > 0 {
			cSeeds, err := r.store.SearchByChunkVector(ctx, qvec, req.SeedK)
			if err != nil {
				return seedResult{}, fmt.Errorf("chunk vector seed search: %w", err)
			}
			chunkW := DefaultChunkVecWeight
			for rank, s := range cSeeds {
				rrfScores[s.ID] += chunkW / float32(rrfK+rank+1)
				if _, ok := originMap[s.ID]; !ok {
					originMap[s.ID] = &seedOrigin{}
				}
				originMap[s.ID].fromV = true
			}
		}

		type rrfEntry struct {
			id    int64
			score float32
		}
		all := make([]rrfEntry, 0, len(rrfScores))
		for id, sc := range rrfScores {
			all = append(all, rrfEntry{id, sc})
		}
		// Tie-break on ID. These slices are built by ranging a map, so equal
		// scores would otherwise be ordered by Go's randomized map iteration and
		// an unstable sort — the same query returning different results on two
		// runs of the same binary, and every before/after comparison carrying an
		// unmeasured noise floor. RRF ties are common: two symbols each seeded by
		// one channel at the same rank score identically.
		sort.Slice(all, func(i, j int) bool {
			if all[i].score != all[j].score {
				return all[i].score > all[j].score
			}
			return all[i].id < all[j].id
		})

		limit := req.SeedK
		if limit > len(all) {
			limit = len(all)
		}
		seeds = make([]int64, limit)
		for i := 0; i < limit; i++ {
			id := all[i].id
			seeds[i] = id
			vectorScores[id] = rrfScores[id]
			originMap[id].score = rrfScores[id]
		}
	}

	return seedResult{
		vSeeds:       vSeeds,
		seeds:        seeds,
		vectorScores: vectorScores,
		originMap:    originMap,
	}, nil
}
