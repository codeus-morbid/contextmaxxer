package retrieve

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

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

		// DECISION(2026-09): a fifth channel that searches the query's CODE
		// IDENTIFIERS alone, at its own depth.
		//
		// The other lexical channels are handed the whole question, which on
		// this benchmark is 850 characters of issue prose. BM25 over a query
		// that long gives a rare identifier almost nothing, and the file it
		// would have pinpointed never enters the pool. Measured directly
		// against FTS: for gold files the pipeline never retrieves even among
		// two hundred results, an identifier-only query puts 75% inside the top
		// fifty, median rank 34 — against median rank 3 for the files we do
		// find. The channel can see them; the way we ask cannot.
		//
		// Its depth is deliberately not req.SeedK. At the default twenty this
		// channel would capture 37% of those files instead of 75%, which is the
		// whole reason they are missing.
		//
		// MEASURED, AND IT DOES NOT WORK. 300 instances, the weight the only
		// difference: File recall 0.556 -> 0.542, paired -0.0135 at t = -2.11,
		// better on 15 instances and worse on 28. Precision and HitRegion did not
		// move. Seeing the files was never the problem — the seed pool holds
		// twenty, and a fifth channel votes candidates out that the other four had
		// right. It is the third time the same mechanism has appeared: merging
		// grep candidates, reranking a deeper pool, and now this all trade a
		// better source for a worse one at a fixed budget.
		// And it is not displacement. Tripling the pool to sixty leaves the shape
		// unchanged — better on 11, worse on 23, against 15 and 28 at twenty —
		// while the pool on its own is neutral (t = -0.45), so nothing was being
		// crowded out and no personalization was diluted. The added signal is
		// simply worse than what it replaces.
		//
		// Which the channel's own numbers explain: the wanted file sits at FTS
		// rank 34, so an OR over eight identifiers ranks THIRTY-THREE wrong files
		// above it, and every one of those votes in the fusion too. Precise about
		// the file we want, noisy about everything else.
		//
		// Stays off, and stays here so the next person to have the idea finds it
		// already measured.
		// REVISIT IF: identifier extraction gets precise enough that the wanted
		// file lands near rank 1 rather than 34.
		if DefaultIdentFTSWeight > 0 {
			if ids := queryIdentifiers(req.Query, identFTSTerms); len(ids) >= 2 {
				iSeeds, err := r.store.SearchByText(ctx, strings.Join(ids, " "), identFTSSeedK)
				if err != nil {
					return seedResult{}, fmt.Errorf("identifier fts seed search: %w", err)
				}
				for rank, s := range iSeeds {
					rrfScores[s.ID] += DefaultIdentFTSWeight / float32(rrfK+rank+1)
					if _, ok := originMap[s.ID]; !ok {
						originMap[s.ID] = &seedOrigin{}
					}
					originMap[s.ID].fromF = true
				}
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

// The identifier channel's knobs. Off by default: it is a measured hypothesis,
// not a shipped default, and CONTEXTMAXXER_IDENT_FTS_WEIGHT turns it on the way
// CONTEXTMAXXER_CHUNK_VEC_WEIGHT does for the chunk-vector channel.
var DefaultIdentFTSWeight float32 = 0

const (
	// identFTSSeedK is how deep this channel looks. The files it exists for sit
	// at a median FTS rank of 34, so twenty would miss half of them by
	// construction.
	identFTSSeedK = 50
	// identFTSTerms caps how many identifiers form the query. Past a handful the
	// OR widens faster than it sharpens.
	identFTSTerms = 8
)

func init() {
	if v := os.Getenv("CONTEXTMAXXER_IDENT_FTS_WEIGHT"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil && f >= 0 {
			DefaultIdentFTSWeight = float32(f)
		}
	}
}
