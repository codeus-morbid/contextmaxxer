package retrieve

import (
	"context"
	"log/slog"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryIdentifiers(t *testing.T) {
	got := queryIdentifiers("`getLatestUndeletedPid` returns null after purge_topic, "+
		"see src/topics/posts.js and the Topic model", 8)

	// Backticked first (the author's own emphasis), then snake_case, then camel.
	assert.Equal(t, "getLatestUndeletedPid", got[0])
	assert.Contains(t, got, "purge_topic")

	// A path is not kept: the store tokenizes it into src OR topics OR posts OR
	// js, which matches hundreds of files and destroys the exactness this
	// channel exists for. Positions are the locator branch's job.
	assert.NotContains(t, got, "src/topics/posts.js")
	for _, id := range got {
		assert.True(t, singleFTSToken(id), "%q must survive as one FTS token", id)
	}
}

func TestLiteralCandidates_RequiresAgreementBetweenIdentifiers(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, QualifiedName: "topics.purge", Kind: "function", StartLine: 1, EndLine: 20, BodyExcerpt: "function purge() {}"},
		{ID: 2, FileID: 11, QualifiedName: "posts.getLatest", Kind: "function", StartLine: 1, EndLine: 20, BodyExcerpt: "function getLatest() {}"},
	}
	ms := &mockStore{
		symbols:   symbols,
		ids:       []int64{1, 2},
		filePaths: map[int64]string{10: "src/topics/purge.js", 11: "src/posts/index.js"},
		// File 10 contains BOTH identifiers, file 11 only one.
		ftsByQuery: map[string][]store.ScoredSymbol{
			"getLatestUndeletedPid": {{Symbol: symbols[0], Score: 2}, {Symbol: symbols[1], Score: 3}},
			"purge_topic":           {{Symbol: symbols[0], Score: 1}},
		},
	}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	got, err := literalCandidates(context.Background(),
		r, "`getLatestUndeletedPid` is wrong after `purge_topic`")
	require.NoError(t, err)

	// Only the file where the two names meet survives, even though the other
	// file scored HIGHER on the single identifier it did match. That is the
	// whole point: this channel scores files, not symbols.
	require.Len(t, got, 1)
	assert.Equal(t, "src/topics/purge.js", got[0].file)
	assert.Equal(t, 2, got[0].matched)
}

func TestReserveLiteralSlots_ReplacesTheTailAndKeepsTheHead(t *testing.T) {
	scored := []ScoredResult{
		{SymbolID: 1, File: "a.go", Score: 9},
		{SymbolID: 2, File: "b.go", Score: 8},
		{SymbolID: 3, File: "c.go", Score: 7},
		{SymbolID: 4, File: "d.go", Score: 6},
		{SymbolID: 5, File: "e.go", Score: 5},
	}
	cands := []literalCandidate{
		{symbol: store.Symbol{ID: 90, QualifiedName: "x.X", BodyExcerpt: "func X() {}"}, file: "x_test.go", score: 3},
		{symbol: store.Symbol{ID: 91, QualifiedName: "y.Y", BodyExcerpt: "func Y() {}"}, file: "y_test.go", score: 2},
		// Already covered by the head: a second symbol in a file the answer
		// shows widens nothing.
		{symbol: store.Symbol{ID: 92, QualifiedName: "a.A2", BodyExcerpt: "func A2() {}"}, file: "a.go", score: 1},
	}

	got := reserveLiteralSlots(scored, cands, 5, 2)

	require.GreaterOrEqual(t, len(got), 5)
	// Head of the answer is untouched...
	assert.Equal(t, int64(1), got[0].SymbolID)
	assert.Equal(t, int64(2), got[1].SymbolID)
	assert.Equal(t, int64(3), got[2].SymbolID)
	// ...and the two slots the ranking had given up on now carry literal hits.
	assert.Equal(t, int64(90), got[3].SymbolID)
	assert.Equal(t, int64(91), got[4].SymbolID)
	assert.Equal(t, "literal_match", got[3].Why)
	// The displaced results stay behind them rather than being dropped: the
	// packer still cuts by budget, and a shorter budget must not lose them here.
	assert.Equal(t, int64(4), got[5].SymbolID)
}

func TestReserveLiteralSlots_OffByDefault(t *testing.T) {
	scored := []ScoredResult{{SymbolID: 1, File: "a.go"}}
	assert.Equal(t, scored, reserveLiteralSlots(scored, nil, 5, 0))
	assert.Equal(t, scored, reserveLiteralSlots(scored, []literalCandidate{
		{symbol: store.Symbol{ID: 90}, file: "x.go"},
	}, 5, 0))
}
