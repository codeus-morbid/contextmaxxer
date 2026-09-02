package retrieve

import (
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// debugAnchors appends a line to the file named by CONTEXTMAXXER_DEBUG_ANCHORS.
// A file and not stderr, because the eval harness discards the served process's
// stderr — which is where a first attempt at this diagnostic disappeared.
func debugAnchors(format string, args ...any) {
	path := os.Getenv("CONTEXTMAXXER_DEBUG_ANCHORS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, format, args...)
}

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
	// anchors are candidates that entered ONLY through graph expansion, tracked
	// so the response can reserve slots for them. Widening the pool alone
	// changed nothing when measured: a neighbour is by nature not lexically
	// similar to the query, so the ranking that missed it misses it again.
	anchors map[int64]bool
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

	// Anchor expansion: put the graph neighbours of the top seeds INTO the
	// candidate pool, rather than only raising their PageRank weight.
	//
	// DECISION(2026-09): measured on SWE-Explore, of the gold files we miss that
	// are present in the index, 64.5% sit one or two call-graph hops from a file
	// we already returned — and the pipeline never considers them, because a
	// neighbour competes on fused score against direct lexical matches and
	// loses. PageRank raises a weight; this adds a candidate. LARGER
	// (arxiv 2605.16352) reports the same mechanism as the largest single
	// contributor to its localization accuracy (-13.5% when removed).
	// ASSUMES: the reranker can reject an irrelevant neighbour, so widening the
	// pool costs ranking quality only where the reranker is wrong.
	// REVISIT IF: precision drops more than reach rises.
	var anchors map[int64]bool
	if req.AnchorExpand > 0 {
		before := len(topIDs)
		// The anchors are the FUSED candidates, not the five top vector seeds.
		// Measured why: with vector seeds as anchors, 7 of 8 neighbours already
		// ranked inside the answer on their own, so expansion added nothing —
		// while the reachability study that motivated this walked from all
		// twenty returned files. Anchoring on what the answer actually contains
		// is what that study measured, and what LARGER anchors on.
		anchorSeeds := make([]store.ScoredSymbol, 0, len(topIDs))
		for _, id := range topIDs {
			anchorSeeds = append(anchorSeeds, store.ScoredSymbol{Symbol: store.Symbol{ID: id}})
		}
		topIDs = appendVectorSeedGraphNeighbors(topIDs, anchorSeeds, g, ranks, len(anchorSeeds), req.AnchorExpand)
		anchors = make(map[int64]bool, len(topIDs)-before)
		for _, id := range topIDs[before:] {
			anchors[id] = true
		}
		debugAnchors("anchors: pool %d -> %d, added %d\n", before, len(topIDs), len(anchors))
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
		anchors:   anchors,
	}
}

// anchorSeedCount is how many top vector seeds act as expansion anchors. Kept
// small on purpose: the point is to follow the few candidates the search is
// most confident about, not to flood the pool from every weak seed.
const anchorSeedCount = 5

// anchorSlotShare is the fraction of the response reserved for graph
// neighbours. A fifth of twenty results is four — enough to carry a chain the
// query could not name, small enough that the ranked answer still leads.
const anchorSlotShare = 5

// reserveAnchorSlots promotes graph-expanded candidates into the tail of the
// response, keeping the ranked order otherwise intact.
//
// DECISION(2026-09): adding neighbours to the candidate POOL changed nothing —
// measured, HitFile 0.373 -> 0.368 — because they are then selected by the same
// ranking that failed to find them. A neighbour reached through the graph is by
// construction not lexically similar to the query; competing on that similarity
// it always loses. So the slots are reserved rather than contested, which is
// what LARGER's anchor expansion does with the agent's context window.
// ASSUMES: a wrong neighbour in the tail costs less than a missing gold file.
// REVISIT IF: precision falls further than reach rises.
func reserveAnchorSlots(scored []ScoredResult, anchors map[int64]bool, maxResults, share int) []ScoredResult {
	if maxResults <= 0 || share <= 0 || len(scored) <= maxResults {
		return scored
	}
	slots := maxResults / share
	if slots == 0 {
		slots = 1
	}

	// Anchors already inside the cut need no help.
	promoted := 0
	for i := 0; i < maxResults && i < len(scored); i++ {
		if anchors[scored[i].SymbolID] {
			promoted++
		}
	}
	present := 0
	for _, s := range scored {
		if anchors[s.SymbolID] {
			present++
		}
	}
	debugAnchors("slots: scored %d, anchors %d, present in scored %d, in cut %d, slots %d\n",
		len(scored), len(anchors), present, promoted, slots)

	if promoted >= slots {
		return scored
	}

	head := append([]ScoredResult(nil), scored[:maxResults-(slots-promoted)]...)
	inHead := make(map[int64]bool, len(head))
	// Which files the answer already covers. A neighbour in a file that is
	// already represented widens nothing: the reachability measurement that
	// motivated this counted only cross-file hops, and a first attempt that
	// promoted same-file neighbours moved distinct files 12.8 -> 12.7, i.e. not
	// at all.
	inFile := make(map[string]bool, len(head))
	for _, s := range head {
		inHead[s.SymbolID] = true
		inFile[s.File] = true
	}
	for _, s := range scored[maxResults-(slots-promoted):] {
		if len(head) >= maxResults {
			break
		}
		if anchors[s.SymbolID] && !inHead[s.SymbolID] && !inFile[s.File] {
			head = append(head, s)
			inHead[s.SymbolID] = true
			inFile[s.File] = true
		}
	}
	// Fill any slot no anchor claimed with the ranking's own next results.
	for _, s := range scored {
		if len(head) >= maxResults {
			break
		}
		if !inHead[s.SymbolID] {
			head = append(head, s)
			inHead[s.SymbolID] = true
		}
	}
	return head
}

// minAnchorAgreement is how many anchors must reach a symbol before it is worth
// adding. 1 accepts every neighbour, which measured monotonically worse; 2 asks
// for corroboration between two independently retrieved places.
var minAnchorAgreement = 2

func init() {
	if v := os.Getenv("CONTEXTMAXXER_ANCHOR_AGREEMENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			minAnchorAgreement = n
		}
	}
}
