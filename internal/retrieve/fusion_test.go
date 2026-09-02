package retrieve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// Anchor expansion adds the graph neighbours of the top seeds to the candidate
// pool. Worked out by hand: node 10 is the seed and is linked to 20 and 30, so
// with a limit of 2 both arrive, in sorted order — the previous implementation
// iterated a map and returned an arbitrary subset, which no measurement could
// reproduce.
// A neighbour is taken only when several anchors agree on it. Worked out by
// hand: anchors 10 and 20; node 30 is reached by both (two votes) and node 40
// only by 10 (one vote), so at the default threshold of two only 30 arrives.
func TestAppendVectorSeedGraphNeighbors_RequiresAgreementBetweenAnchors(t *testing.T) {
	g := &Graph{
		NumNodes: 4,
		NodeIDs:  []int64{10, 20, 30, 40},
		NodeIdx:  map[int64]int{10: 0, 20: 1, 30: 2, 40: 3},
		// 10 -> 30, 40 ; 20 -> 30
		OutEdges: [][]int{{2, 3}, {2}, {0, 1}, {0}},
	}
	anchors := []store.ScoredSymbol{
		{Symbol: store.Symbol{ID: 10}},
		{Symbol: store.Symbol{ID: 20}},
	}

	got := appendVectorSeedGraphNeighbors([]int64{10, 20}, anchors, g, nil, 2, 5)
	require.Equal(t, []int64{10, 20, 30}, got, "40 has only one anchor behind it")

	// Same inputs, same output: the earlier version iterated a map and returned
	// an arbitrary subset, which no measurement could reproduce.
	require.Equal(t, got, appendVectorSeedGraphNeighbors([]int64{10, 20}, anchors, g, nil, 2, 5))

	// Off by default.
	require.Equal(t, []int64{10, 20}, appendVectorSeedGraphNeighbors([]int64{10, 20}, anchors, g, nil, 2, 0))
}

// Lowering the threshold to one accepts every neighbour — the behaviour that
// measured monotonically worse, kept reachable for comparison.
func TestAppendVectorSeedGraphNeighbors_ThresholdOneAcceptsEveryNeighbour(t *testing.T) {
	old := minAnchorAgreement
	minAnchorAgreement = 1
	defer func() { minAnchorAgreement = old }()

	g := &Graph{
		NumNodes: 3,
		NodeIDs:  []int64{10, 20, 30},
		NodeIdx:  map[int64]int{10: 0, 20: 1, 30: 2},
		OutEdges: [][]int{{1, 2}, {0}, {0}},
	}
	seeds := []store.ScoredSymbol{{Symbol: store.Symbol{ID: 10}}}
	require.Equal(t, []int64{10, 20, 30}, appendVectorSeedGraphNeighbors([]int64{10}, seeds, g, nil, 5, 2))
}

// Reserved slots must hold a graph neighbour that the ranking buried — but only
// when it brings a NEW file. Worked out by hand: max 5, share 5 -> 1 slot; the
// ranking gives 1..7 across distinct files with the only anchor at 7, so the
// answer keeps 1-4 and takes 7.
func TestReserveAnchorSlots_PromotesANeighbourInANewFile(t *testing.T) {
	scored := []ScoredResult{
		{SymbolID: 1, File: "a.go"}, {SymbolID: 2, File: "b.go"},
		{SymbolID: 3, File: "c.go"}, {SymbolID: 4, File: "d.go"},
		{SymbolID: 5, File: "e.go"}, {SymbolID: 6, File: "f.go"},
		{SymbolID: 7, File: "g.go"},
	}
	got := reserveAnchorSlots(scored, map[int64]bool{7: true}, 5, 5)

	ids := make([]int64, len(got))
	for i, s := range got {
		ids[i] = s.SymbolID
	}
	require.Equal(t, []int64{1, 2, 3, 4, 7}, ids)
}

// A neighbour in a file the answer already covers widens nothing, so it must
// not take the slot. This is the case that made the first implementation
// useless: promoting same-file neighbours left distinct files at 12.8 -> 12.7.
func TestReserveAnchorSlots_SkipsANeighbourInAFileAlreadyCovered(t *testing.T) {
	scored := []ScoredResult{
		{SymbolID: 1, File: "a.go"}, {SymbolID: 2, File: "b.go"},
		{SymbolID: 3, File: "c.go"}, {SymbolID: 4, File: "d.go"},
		{SymbolID: 5, File: "e.go"}, {SymbolID: 6, File: "f.go"},
		{SymbolID: 7, File: "a.go"},
	}
	got := reserveAnchorSlots(scored, map[int64]bool{7: true}, 5, 5)

	ids := make([]int64, len(got))
	for i, s := range got {
		ids[i] = s.SymbolID
	}
	require.Equal(t, []int64{1, 2, 3, 4, 5}, ids, "the slot falls back to the ranking")
}

// An anchor that already ranked well needs no slot.
func TestReserveAnchorSlots_LeavesAWellRankedAnchorAlone(t *testing.T) {
	scored := []ScoredResult{
		{SymbolID: 1, File: "a.go"}, {SymbolID: 2, File: "b.go"},
		{SymbolID: 3, File: "c.go"}, {SymbolID: 4, File: "d.go"},
		{SymbolID: 5, File: "e.go"}, {SymbolID: 6, File: "f.go"},
	}
	got := reserveAnchorSlots(scored, map[int64]bool{2: true}, 5, 5)
	require.Equal(t, int64(1), got[0].SymbolID)
	require.Equal(t, int64(5), got[4].SymbolID)
}

// With no anchor to promote the response is never shorter than it would be.
func TestReserveAnchorSlots_FillsUnclaimedSlotsFromTheRanking(t *testing.T) {
	scored := []ScoredResult{
		{SymbolID: 1, File: "a.go"}, {SymbolID: 2, File: "b.go"},
		{SymbolID: 3, File: "c.go"}, {SymbolID: 4, File: "d.go"},
		{SymbolID: 5, File: "e.go"}, {SymbolID: 6, File: "f.go"},
	}
	got := reserveAnchorSlots(scored, map[int64]bool{}, 5, 5)
	require.Len(t, got, 5)
	require.Equal(t, int64(5), got[4].SymbolID)
}
