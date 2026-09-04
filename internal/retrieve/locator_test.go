package retrieve

import (
	"context"
	"log/slog"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLocator(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  locator
		ok    bool
	}{
		{"rg -n output", "src/topics/posts.js:142", locator{path: "src/topics/posts.js", line: 142}, true},
		{"rg -n with the matched line attached", "src/topics/posts.js:142:  const pid = 1", locator{path: "src/topics/posts.js", line: 142}, true},
		{"explicit span keeps the start", "internal/retrieve/pipeline.go:142-190", locator{path: "internal/retrieve/pipeline.go", line: 142}, true},
		{"whole file", "internal/retrieve/pipeline.go", locator{path: "internal/retrieve/pipeline.go"}, true},
		{"windows separators", `internal\retrieve\pipeline.go:12`, locator{path: "internal/retrieve/pipeline.go", line: 12}, true},
		{"leading ./ trimmed", "./cmd/main.go:3", locator{path: "cmd/main.go", line: 3}, true},
		{"bare filename with extension", "pipeline.go:7", locator{path: "pipeline.go", line: 7}, true},

		// Everything below must stay a semantic search.
		{"prose with a colon", "Config: how do we load it", locator{}, false},
		{"prose that starts with a dotted word", "e.g: the parser fails", locator{}, false},
		{"plain question", "where is the retry budget applied", locator{}, false},
		{"single word", "PageRank", locator{}, false},
		{"empty", "   ", locator{}, false},
		{"zero line is not a position", "cmd/main.go:0", locator{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseLocator(tc.query)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

// countingEmbedder fails the point of the branch loudly: a locator is answered
// from the index, so the embedder must not be reached at all.
type countingEmbedder struct{ calls int }

func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func locatorTestStore() *mockStore {
	// alpha.go holds a class spanning the file with a method inside it, so the
	// innermost-first rule has something to be wrong about.
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Poster", Kind: "class", QualifiedName: "topics.Poster", StartLine: 1, EndLine: 100, BodyExcerpt: "class Poster {}"},
		{ID: 2, FileID: 10, Name: "getLatestUndeletedPid", Kind: "method", QualifiedName: "topics.Poster.getLatestUndeletedPid", StartLine: 40, EndLine: 60, BodyExcerpt: "getLatestUndeletedPid() {}"},
		{ID: 3, FileID: 10, Name: "purge", Kind: "method", QualifiedName: "topics.Poster.purge", StartLine: 70, EndLine: 90, BodyExcerpt: "purge() {}"},
		{ID: 4, FileID: 11, Name: "Other", Kind: "function", QualifiedName: "other.Other", StartLine: 1, EndLine: 5, BodyExcerpt: "function Other() {}"},
	}
	return &mockStore{
		symbols:   symbols,
		ids:       []int64{1, 2, 3, 4},
		filePaths: map[int64]string{10: "src/topics/posts.js", 11: "src/other.js"},
	}
}

func TestRetriever_LocatorAnswersWithoutEmbedding(t *testing.T) {
	emb := &countingEmbedder{}
	r := NewRetriever(locatorTestStore(), emb, slog.Default())

	res, err := r.Retrieve(context.Background(), Request{
		Query:        "src/topics/posts.js:45",
		BudgetTokens: 10000,
		MaxResults:   5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Symbols)

	assert.Equal(t, 0, emb.calls, "a locator query must not reach the embedder")
	assert.Equal(t, "locator", res.Symbols[0].Why)
	// Line 45 is inside the method, which is inside the class: the method is
	// what the agent pointed at, the class is context for it.
	assert.Equal(t, "topics.Poster.getLatestUndeletedPid", res.Symbols[0].QualifiedName)
	assert.Equal(t, "src/topics/posts.js", res.Symbols[0].File)
}

func TestRetriever_LocatorAcceptsRawGrepLine(t *testing.T) {
	r := NewRetriever(locatorTestStore(), &countingEmbedder{}, slog.Default())

	res, err := r.Retrieve(context.Background(), Request{
		Query:        "src/topics/posts.js:75:\tconst pid = await this.getLatestUndeletedPid();",
		BudgetTokens: 10000,
		MaxResults:   5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Symbols)
	assert.Equal(t, "topics.Poster.purge", res.Symbols[0].QualifiedName)
}

func TestRetriever_LocatorFallsThroughWhenPathIsUnknown(t *testing.T) {
	emb := &countingEmbedder{}
	r := NewRetriever(locatorTestStore(), emb, slog.Default())

	res, err := r.Retrieve(context.Background(), Request{
		Query:        "vendor/nowhere/missing.go:12",
		BudgetTokens: 10000,
		MaxResults:   5,
	})
	require.NoError(t, err)
	// The regex proposed a locator and the index refused it, so the query must
	// have been served as an ordinary search rather than answered as a position.
	assert.Positive(t, emb.calls, "an unresolved path must fall through to semantic search")
	for _, s := range res.Symbols {
		assert.NotEqual(t, "locator", s.Why)
	}
}

func TestSelectLocatorSymbols_NearestWhenNothingEncloses(t *testing.T) {
	syms := []store.Symbol{
		{ID: 1, FileID: 10, StartLine: 40, EndLine: 60},
		{ID: 2, FileID: 10, StartLine: 70, EndLine: 90},
	}
	// Line 5 is the import block: no symbol contains it, and an agent that
	// pasted a real position should still be told what is around it.
	got := selectLocatorSymbols(syms, []int64{10}, 5, 5)
	require.Len(t, got, 2)
	assert.Equal(t, int64(1), got[0].ID)
}

func TestResolveLocator_SuffixMatchIsDeterministic(t *testing.T) {
	paths := map[int64]string{
		1: "packages/server/src/topics/posts.js",
		2: "packages/client/src/topics/posts.js",
		3: "src/other.js",
	}
	// An agent that ran grep from a subdirectory pastes a relative path that
	// matches more than one indexed file; the order must not come from map
	// iteration.
	got := resolveLocator(locator{path: "src/topics/posts.js"}, paths)
	require.Len(t, got, 2)
	assert.Equal(t, []int64{2, 1}, got)
}
