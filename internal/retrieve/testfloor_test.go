package retrieve

import (
	"testing"

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
