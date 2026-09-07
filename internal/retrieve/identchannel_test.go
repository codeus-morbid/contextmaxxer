package retrieve

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The channel exists because the other lexical channels are handed the whole
// question. This drives that difference directly: the symbol that answers is
// reachable ONLY by the identifier query, and invisible to a search for the
// prose around it.
func identChannelStore() (*mockStore, []store.Symbol) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, QualifiedName: "topics.getLatestUndeletedPid", Kind: "function",
			StartLine: 1, EndLine: 30, BodyExcerpt: "function getLatestUndeletedPid() { return pid; } // plus more body to clear the micro filter"},
		{ID: 2, FileID: 11, QualifiedName: "docs.overview", Kind: "function",
			StartLine: 1, EndLine: 30, BodyExcerpt: "the topic list sometimes shows a deleted post after purging, which is confusing"},
	}
	prose := "Deleted posts still appear in the topic list. Steps to reproduce: purge a post, " +
		"then reload. Expected behaviour: it disappears. See `getLatestUndeletedPid` and purge_topic."
	ms := &mockStore{
		symbols:   symbols,
		ids:       []int64{1, 2},
		filePaths: map[int64]string{10: "src/topics/posts.js", 11: "docs/overview.md"},
		ftsByQuery: map[string][]store.ScoredSymbol{
			// Asked with the whole issue, BM25 finds the prose, not the code.
			prose: {{Symbol: symbols[1], Score: 3}},
		},
		// Nor does the vector channel reach the code symbol: the query is a
		// description of a symptom.
		vecResult: []store.ScoredSymbol{{Symbol: symbols[1], Score: 0.8}},
	}
	return ms, symbols
}

func TestIdentifierChannel_OffByDefault(t *testing.T) {
	require.Zero(t, DefaultIdentFTSWeight,
		"an unproven channel ships off, like the chunk-vector one")
}

func TestIdentifierChannel_ReachesWhatProseDoesNot(t *testing.T) {
	ms, symbols := identChannelStore()
	prose := "Deleted posts still appear in the topic list. Steps to reproduce: purge a post, " +
		"then reload. Expected behaviour: it disappears. See `getLatestUndeletedPid` and purge_topic."

	// The identifier query is what the channel asks with; wire the mock to
	// answer exactly that string, so the test fails if the extraction or the
	// join ever changes shape.
	ids := queryIdentifiers(prose, identFTSTerms)
	require.Contains(t, ids, "getLatestUndeletedPid")
	ms.ftsByQuery[strings.Join(ids, " ")] = []store.ScoredSymbol{{Symbol: symbols[0], Score: 9}}

	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())
	req := Request{Query: prose, BudgetTokens: 10000, SeedK: 5, MaxResults: 5, Mode: ModeHybrid}

	// What is asserted here is the channel's own behaviour, not the ranking's:
	// in an index this small every symbol reaches the answer through PageRank
	// whatever the seeds were, so "unreachable when off" would be a statement
	// about the fixture rather than about the code.
	t.Run("off: the identifier query is never asked", func(t *testing.T) {
		asked := map[string]bool{}
		ms.onSearchByText = func(q string) { asked[q] = true }
		t.Cleanup(func() { ms.onSearchByText = nil })

		_, err := r.Retrieve(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, asked[strings.Join(ids, " ")],
			"with the channel off nothing should ask FTS for the identifiers alone")
		assert.True(t, asked[prose], "the prose channels still run")
	})

	t.Run("on: it is asked, and what it finds is seeded", func(t *testing.T) {
		old := DefaultIdentFTSWeight
		DefaultIdentFTSWeight = 1
		t.Cleanup(func() { DefaultIdentFTSWeight = old })

		asked := map[string]bool{}
		ms.onSearchByText = func(q string) { asked[q] = true }
		t.Cleanup(func() { ms.onSearchByText = nil })

		res, err := r.Retrieve(context.Background(), req)
		require.NoError(t, err)
		assert.True(t, asked[strings.Join(ids, " ")], "the channel must ask with the identifiers alone")
		assert.Contains(t, resultNames(res.Symbols), "topics.getLatestUndeletedPid")
	})
}

func TestIdentifierChannel_NeedsTwoIdentifiers(t *testing.T) {
	// One identifier is a word, not evidence: a bare OR over it would widen the
	// pool without sharpening it.
	old := DefaultIdentFTSWeight
	DefaultIdentFTSWeight = 1
	t.Cleanup(func() { DefaultIdentFTSWeight = old })

	ms, _ := identChannelStore()
	asked := map[string]bool{}
	ms.ftsByQuery = map[string][]store.ScoredSymbol{}
	ms.onSearchByText = func(q string) { asked[q] = true }

	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())
	_, err := r.Retrieve(context.Background(), Request{
		Query: "the retry budget is wrong", BudgetTokens: 10000, SeedK: 5, MaxResults: 5,
	})
	require.NoError(t, err)
	for q := range asked {
		assert.NotEqual(t, "budget", q, "a single identifier must not become a channel query")
	}
}

func resultNames(rs []ScoredResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.QualifiedName
	}
	return out
}
