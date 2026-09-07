package retrieve

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func names(rs []ScoredResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.QualifiedName
	}
	return out
}

func TestPreferNestedSymbols(t *testing.T) {
	// The measured case: a 296-line class outranks the 46-line method inside it
	// that actually holds the answer, and the shown window then falls elsewhere
	// in the class.
	class := ScoredResult{SymbolID: 1, QualifiedName: "topics.Poster", File: "a.py", StartLine: 1, EndLine: 300}
	method := ScoredResult{SymbolID: 2, QualifiedName: "topics.Poster.purge", File: "a.py", StartLine: 120, EndLine: 166}
	other := ScoredResult{SymbolID: 3, QualifiedName: "other.helper", File: "b.py", StartLine: 1, EndLine: 20}

	t.Run("the member comes first", func(t *testing.T) {
		got := preferNestedSymbols([]ScoredResult{class, method, other})
		assert.Equal(t, []string{"topics.Poster.purge", "topics.Poster", "other.helper"}, names(got))
	})

	t.Run("nothing is dropped", func(t *testing.T) {
		got := preferNestedSymbols([]ScoredResult{class, method, other})
		assert.Len(t, got, 3, "a class is context, not noise: it moves, it does not go")
	})

	t.Run("an already-correct order is untouched", func(t *testing.T) {
		in := []ScoredResult{method, class, other}
		assert.Equal(t, names(in), names(preferNestedSymbols(in)))
	})

	t.Run("unrelated results keep their ranked order", func(t *testing.T) {
		a := ScoredResult{SymbolID: 10, QualifiedName: "p.A", File: "x.py", StartLine: 1, EndLine: 50}
		b := ScoredResult{SymbolID: 11, QualifiedName: "p.B", File: "x.py", StartLine: 60, EndLine: 90}
		got := preferNestedSymbols([]ScoredResult{a, b})
		assert.Equal(t, []string{"p.A", "p.B"}, names(got), "overlapping nothing, so nothing moves")
	})

	t.Run("same span is not containment", func(t *testing.T) {
		// Two extractors can emit the same range; neither is finer than the
		// other, so the ranking's own order stands.
		x := ScoredResult{SymbolID: 20, QualifiedName: "p.X", File: "y.py", StartLine: 5, EndLine: 25}
		y := ScoredResult{SymbolID: 21, QualifiedName: "p.Y", File: "y.py", StartLine: 5, EndLine: 25}
		got := preferNestedSymbols([]ScoredResult{x, y})
		assert.Equal(t, []string{"p.X", "p.Y"}, names(got))
	})

	t.Run("a different file is never containment", func(t *testing.T) {
		wide := ScoredResult{SymbolID: 30, QualifiedName: "p.Wide", File: "one.py", StartLine: 1, EndLine: 400}
		narrow := ScoredResult{SymbolID: 31, QualifiedName: "p.Narrow", File: "two.py", StartLine: 10, EndLine: 20}
		got := preferNestedSymbols([]ScoredResult{wide, narrow})
		assert.Equal(t, []string{"p.Wide", "p.Narrow"}, names(got))
	})

	t.Run("three levels put the innermost first", func(t *testing.T) {
		outer := ScoredResult{SymbolID: 40, QualifiedName: "m.Outer", File: "z.py", StartLine: 1, EndLine: 300}
		mid := ScoredResult{SymbolID: 41, QualifiedName: "m.Outer.Mid", File: "z.py", StartLine: 50, EndLine: 200}
		inner := ScoredResult{SymbolID: 42, QualifiedName: "m.Outer.Mid.Leaf", File: "z.py", StartLine: 100, EndLine: 140}
		got := names(preferNestedSymbols([]ScoredResult{outer, mid, inner}))
		assert.Equal(t, "m.Outer.Mid.Leaf", got[0])
		assert.Equal(t, "m.Outer", got[2], "the widest span ends up last of the three")
	})
}
