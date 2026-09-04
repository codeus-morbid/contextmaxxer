package retrieve

import (
	"context"
	"log/slog"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWalkerTestFile_CoversTheConventionsTheSampleUses(t *testing.T) {
	// The retrieval package's own isTestFile knows Go and TypeScript only. This
	// experiment's sample is mostly Python, so a knob built on that predicate
	// would have been a no-op on the instances it was meant to fix.
	for _, p := range []string{
		"tests/test_config.py", "xarray/tests/dataset_test.py",
		"internal/store/sqlite/sqlite_test.go", "src/api.spec.ts",
		"packages/preact/test/render.test.js", "spec/models/user_spec.rb",
		"src/test/java/org/x/ParserTest.java",
	} {
		assert.True(t, walkerTestFile(p), "%q should count as a test file", p)
	}
	for _, p := range []string{
		"xarray/core/dataset.py", "internal/store/sqlite/sqlite.go",
		"src/api.ts", "src/latest.java", "protest.py",
	} {
		assert.False(t, walkerTestFile(p), "%q is not a test file", p)
	}
}

func TestApplyTestFloor(t *testing.T) {
	scored := []ScoredResult{
		{SymbolID: 1, File: "pkg/impl_a.py"},
		{SymbolID: 2, File: "tests/test_a.py"},
		{SymbolID: 3, File: "pkg/impl_b.py"},
		{SymbolID: 4, File: "tests/test_b.py"},
		{SymbolID: 5, File: "pkg/impl_c.py"},
	}

	t.Run("off by default", func(t *testing.T) {
		assert.Equal(t, scored, applyTestFloor(scored, 0))
	})

	t.Run("reserves the top slots for implementation", func(t *testing.T) {
		got := applyTestFloor(scored, 2)
		require.Len(t, got, 5)
		assert.Equal(t, []int64{1, 3, 2, 4, 5}, ids(got))
	})

	t.Run("a floor past the answer puts every test last", func(t *testing.T) {
		got := applyTestFloor(scored, 40)
		assert.Equal(t, []int64{1, 3, 5, 2, 4}, ids(got))
	})

	t.Run("tests still answer when implementation runs out", func(t *testing.T) {
		// Two implementation results and a floor of five: the tests are not
		// dropped, they fill what implementation did not.
		short := []ScoredResult{
			{SymbolID: 1, File: "tests/test_a.py"},
			{SymbolID: 2, File: "pkg/impl_a.py"},
			{SymbolID: 3, File: "tests/test_b.py"},
		}
		got := applyTestFloor(short, 5)
		assert.Equal(t, []int64{2, 1, 3}, ids(got))
	})
}

func ids(rs []ScoredResult) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.SymbolID
	}
	return out
}

func TestDefaultTestFloor(t *testing.T) {
	// A quarter of the answer: the sweep measured 10 of 40, and the served
	// max_results is not the probe's, so the ratio travels and the count does
	// not.
	assert.Equal(t, 10, defaultTestFloor(40))
	assert.Equal(t, 5, defaultTestFloor(20))
	assert.Equal(t, 1, defaultTestFloor(5))
	// Too small to reserve anything without deciding the whole answer.
	assert.Equal(t, 0, defaultTestFloor(3))
	assert.Equal(t, 0, defaultTestFloor(1))
}

func TestRetriever_TestFloorIsOnByDefaultAndCanBeSwitchedOff(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, QualifiedName: "tests.test_a", Kind: "function", StartLine: 1, EndLine: 20, BodyExcerpt: "def test_a(): pass and more body here to clear the micro filter"},
		{ID: 2, FileID: 11, QualifiedName: "pkg.impl", Kind: "function", StartLine: 1, EndLine: 20, BodyExcerpt: "def impl(): pass and more body here to clear the micro filter"},
	}
	newStore := func() *mockStore {
		return &mockStore{
			symbols:   symbols,
			ids:       []int64{1, 2},
			filePaths: map[int64]string{10: "tests/test_a.py", 11: "pkg/impl.py"},
			// The test file is the stronger lexical match, so without a floor
			// it leads.
			ftsResult: []store.ScoredSymbol{{Symbol: symbols[0], Score: 9}, {Symbol: symbols[1], Score: 1}},
		}
	}

	r := NewRetriever(newStore(), &mockEmbedder{}, slog.Default())
	res, err := r.Retrieve(context.Background(), Request{Query: "test a", BudgetTokens: 10000, MaxResults: 8})
	require.NoError(t, err)
	require.Len(t, res.Symbols, 2)
	assert.Equal(t, "pkg.impl", res.Symbols[0].QualifiedName, "implementation leads by default")

	off := NewRetriever(newStore(), &mockEmbedder{}, slog.Default())
	resOff, err := off.Retrieve(context.Background(), Request{Query: "test a", BudgetTokens: 10000, MaxResults: 8, TestFloor: -1})
	require.NoError(t, err)
	require.Len(t, resOff.Symbols, 2)
	assert.Equal(t, "tests.test_a", resOff.Symbols[0].QualifiedName, "a negative floor switches it off")
}
