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
