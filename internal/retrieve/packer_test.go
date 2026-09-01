package retrieve

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A compacted result must report the lines it actually shows. Worked out by
// hand: the symbol spans 100-180, the compact form is a signature plus one
// docstring line, so two lines are visible — 100-101 — and 79 are omitted after.
func TestPack_CompactedResultReportsTheLinesItShows(t *testing.T) {
	full := ScoredResult{
		QualifiedName: "pkg.Wide",
		Signature:     "func Wide(x int) error",
		Docstring:     "does the thing",
		Body:          strings.Repeat("line\n", 81),
		StartLine:     100,
		EndLine:       180,
		BodyStartLine: 100,
		BodyEndLine:   180,
	}
	top := full
	top.QualifiedName = "pkg.Top"

	// fullBodyCount 1: the second result is the compacted one.
	packed, _ := Pack([]ScoredResult{top, full}, 32000, 1)
	require.Len(t, packed, 2)

	compact := packed[1]
	require.Equal(t, "compact", compact.Detail)
	require.Equal(t, 100, compact.BodyStartLine)
	require.Equal(t, 101, compact.BodyEndLine, "signature + one docstring line is two lines, not eighty-one")
	require.Equal(t, "full", packed[0].Detail)
	require.Equal(t, 180, packed[0].BodyEndLine, "the full result is untouched")
}
