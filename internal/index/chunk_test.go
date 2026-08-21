package index

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/require"
)

func numberedBody(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d()", i)
	}
	return strings.Join(lines, "\n")
}

// Worked by hand against the shape that motivated chunking: Head.Init is 260
// lines starting at 694, so with a stride of 30 the windows start at 0, 30, ...
// 240 — nine of them — and a probe 57 lines into the body falls inside the
// second window rather than off the end of the excerpt.
func TestChunkSymbolCoversTheWholeBody(t *testing.T) {
	sym := store.Symbol{
		ID: 7, QualifiedName: "tsdb.Head.Init", Kind: "method",
		StartLine: 694, EndLine: 953,
		BodyExcerpt: "capped", FullBody: numberedBody(260),
	}

	chunks := ChunkSymbol("tsdb/head.go", "go", sym)

	require.Len(t, chunks, 9)
	require.Equal(t, 694, chunks[0].StartLine)
	require.Equal(t, 724, chunks[1].StartLine)
	require.Equal(t, int64(7), chunks[0].SymbolID)

	// Every line of the body must live in some chunk, or the blind spot simply
	// moves instead of closing.
	for i := 0; i < 260; i++ {
		needle := fmt.Sprintf("line%d()", i)
		found := false
		for _, c := range chunks {
			if strings.Contains(c.Text, needle+"\n") || strings.HasSuffix(c.Text, needle) {
				found = true
				break
			}
		}
		require.True(t, found, "body line %d is in no chunk", i)
	}

	// The header anchors a bare window of code to its symbol.
	// identifierWords normalizes to lowercase words, matching the profile the
	// per-symbol embedding text uses.
	require.Contains(t, chunks[3].Text, "tsdb head init")
	require.Contains(t, chunks[3].Text, "method")
}

func TestChunkSymbolSkipsUncappedAndUnwindowable(t *testing.T) {
	// Not capped: already fully represented by its own vector.
	require.Nil(t, ChunkSymbol("a.go", "go", store.Symbol{
		QualifiedName: "pkg.Small", BodyExcerpt: numberedBody(10),
	}))
	// Capped but a single line: there is no window to cut.
	require.Nil(t, ChunkSymbol("a.go", "go", store.Symbol{
		QualifiedName: "pkg.Minified", BodyExcerpt: "x", FullBody: strings.Repeat("a;", 5000),
	}))
}

// A capped body of few but long lines still hides code past the cap; an earlier
// "too few lines to bother" guard skipped exactly those symbols.
func TestChunkSymbolChunksShortBodiesWithLongLines(t *testing.T) {
	long := strings.Repeat(strings.Repeat("x", 200)+"\n", 30)
	chunks := ChunkSymbol("a.go", "go", store.Symbol{
		ID: 3, QualifiedName: "pkg.Wide", Kind: "function", StartLine: 10,
		BodyExcerpt: "capped", FullBody: long,
	})

	require.NotEmpty(t, chunks)
	for _, c := range chunks {
		require.LessOrEqual(t, len(c.Text), chunkMaxBytes+200,
			"a window of long lines must stay bounded (body cap plus its header)")
	}
}

// One switch drives both sides. They must not disagree: chunks nothing reads
// are wasted indexing time, and a weight pointing at chunks that were never
// built is a silent no-op.
func TestChunkingEnabledFollowsTheRetrievalWeight(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"0.0", false},
		{"not-a-number", false},
		{"0.6", true},
		{"1.0", true},
	} {
		t.Setenv("CONTEXTMAXXER_CHUNK_VEC_WEIGHT", tc.env)
		require.Equal(t, tc.want, ChunkingEnabled(), "env %q", tc.env)
	}
}
