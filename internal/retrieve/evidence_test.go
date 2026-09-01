package retrieve

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyEvidenceSpans_DegradedPathDoesNotClaimAFullBody(t *testing.T) {
	// When the embedder fails, runPipeline logs "serving FTS-only seeds" and
	// carries on with a nil query vector. applyEvidenceSpans then returns
	// early — and used to leave a capped symbol reporting its FULL line range
	// with Detail "full", holding three lines of a two-hundred-line function
	// and an unflagged "// ... [truncated]" inside the text.
	r := NewRetriever(&mockStore{}, &mockEmbedder{}, slog.Default())
	results := []ScoredResult{{
		QualifiedName: "pkg.Huge",
		StartLine:     100,
		EndLine:       300,
		Detail:        "full",
		Body:          "func Huge() {\n\tstep()\n" + truncationMarker,
	}}

	applyEvidenceSpans(context.Background(), r, "anything", nil, results)

	require.Equal(t, "excerpt", results[0].Detail,
		"a body still carrying the truncation marker is an excerpt, whatever the packer labelled it")
	require.Less(t, results[0].BodyEndLine, 300,
		"the reported range must match the lines actually shipped, not the symbol's full extent")
	require.Equal(t, 100, results[0].BodyStartLine)
}

// The span a compacted result reports must survive evidence selection. That
// loop reset every result to the symbol's full extent, which undid the packer
// and re-announced eighty lines as visible when two were sent.
func TestApplyEvidenceSpans_LeavesCompactedSpansAlone(t *testing.T) {
	results := []ScoredResult{
		{Detail: "compact", Body: "func Wide(x int) error", StartLine: 100, EndLine: 180, BodyStartLine: 100, BodyEndLine: 101},
		{Detail: "full", Body: "func Top() {}", StartLine: 10, EndLine: 40, BodyStartLine: 10, BodyEndLine: 40},
	}

	// A nil embedder returns right after the loop under test.
	applyEvidenceSpans(context.Background(), &Retriever{}, "query", nil, results)

	require.Equal(t, 100, results[0].BodyStartLine)
	require.Equal(t, 101, results[0].BodyEndLine, "the compacted span must not be widened back to the symbol")
	require.Equal(t, 40, results[1].BodyEndLine, "a full result still spans its symbol")
}
