package retrieve

import (
	"sort"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// fusedRanking is what the fusion phase hands to candidate hydration.
type fusedRanking struct {
	topIDs    []int64
	topScores map[int64]float32
	seedRank  map[int64]int
	pprRank   map[int64]int
	maxDegree int
	// maxPPR is carried out because the feature vector built after this phase
	// normalises each candidate's PageRank score against it.
	maxPPR float32
}

// fuseSeedAndPPR normalises the seed and PageRank scores, combines them under
// alpha, and picks the candidate pool — including the protections that keep
// strong vector seeds and strong PPR-only candidates from being crowded out.
//
// Extracted from runPipeline verbatim; moved, not rewritten, and checked by
// running the 20-repo gate before and after. The only edit to the body is that
// qTokens is now computed by the caller and passed in, because it is the one
// value the block produced that the code after it also wanted.
func fuseSeedAndPPR(
	r *Retriever, req Request, g *Graph,
	seeds []int64, seedScores, ranks map[int64]float32,
	vSeeds []store.ScoredSymbol, alpha float32, qTokens []string,
) fusedRanking {
	var maxSeed float32
	for _, sc := range seedScores {
		if sc > maxSeed {
			maxSeed = sc
		}
	}
	var maxPPR float32
	for _, sc := range ranks {
		if sc > maxPPR {
			maxPPR = sc
		}
	}
	var maxDegree int
	for _, out := range g.OutEdges {
		if len(out) > maxDegree {
			maxDegree = len(out)
		}
	}
	seedRank := make(map[int64]int, len(seeds))
	for i, id := range seeds {
		seedRank[id] = i + 1
	}
	type pprEntry struct {
		id    int64
		score float32
	}
	pprEntries := make([]pprEntry, 0, len(ranks))
	for id, sc := range ranks {
		pprEntries = append(pprEntries, pprEntry{id: id, score: sc})
	}
	sort.Slice(pprEntries, func(i, j int) bool {
		if pprEntries[i].score != pprEntries[j].score {
			return pprEntries[i].score > pprEntries[j].score
		}
		return pprEntries[i].id < pprEntries[j].id
	})
	pprRank := make(map[int64]int, len(pprEntries))
	for i, entry := range pprEntries {
		pprRank[entry.id] = i + 1
	}

	type ranked struct {
		id    int64
		score float32
	}
	allRanked := make([]ranked, 0, len(ranks))
	fusedScores := make(map[int64]float32, len(ranks))
	for id, pprSc := range ranks {
		var normSeed float32
		if maxSeed > 0 {
			if sc, isSeed := seedScores[id]; isSeed {
				normSeed = sc / maxSeed
			}
		}
		var normPPR float32
		if maxPPR > 0 {
			normPPR = pprSc / maxPPR
		}
		fused := alpha*normSeed + (1-alpha)*normPPR
		fusedScores[id] = fused
		allRanked = append(allRanked, ranked{id, fused})
	}
	sort.Slice(allRanked, func(i, j int) bool {
		if allRanked[i].score != allRanked[j].score {
			return allRanked[i].score > allRanked[j].score
		}
		return allRanked[i].id < allRanked[j].id
	})

	limit := req.MaxResults
	if r.reranker != nil && req.RerankK > limit {
		limit = req.RerankK
	}
	if limit > len(allRanked) {
		limit = len(allRanked)
	}
	topIDs := make([]int64, limit)
	for i := 0; i < limit; i++ {
		topIDs[i] = allRanked[i].id
	}
	if !req.AlphaSet {
		if r.reranker != nil {
			topIDs = appendMissingTopVectorSeeds(topIDs, vSeeds, limit, qTokens)
		} else {
			topIDs = protectTopVectorSeeds(topIDs, vSeeds, 1)
		}
	}

	// DECISION: Re-protect top-PPR-ranked candidates (Bug B fix). FTS in hybrid
	// mode crowds out PPR-only graph candidates with low seed score; this ensures
	// strong PPR candidates always reach the reranker pool.
	const protectPPRCount = 5
	pprIDsByRank := make([]int64, 0, len(pprEntries))
	for _, e := range pprEntries {
		pprIDsByRank = append(pprIDsByRank, e.id)
	}
	topIDs = appendMissingTopPPR(topIDs, pprIDsByRank, protectPPRCount)

	topScores := make(map[int64]float32, len(topIDs))
	for _, id := range topIDs {
		topScores[id] = fusedScores[id]
	}

	return fusedRanking{
		topIDs:    topIDs,
		topScores: topScores,
		seedRank:  seedRank,
		pprRank:   pprRank,
		maxDegree: maxDegree,
		maxPPR:    maxPPR,
	}
}
