package mcp

import (
	"strings"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/stretchr/testify/require"
)

// numberBody handles segmented bodies and is tested directly. What was never
// tested is that the ENCODING the server actually serves passes the segments
// in: markdown is the default, and it called numberLines, which is numberBody
// with a nil segment list.
func TestMarkdownNumbersSegmentedBodyLikeJSON(t *testing.T) {
	body := strings.Join([]string{
		"first", "second",
		retrieve.EvidenceGapMarker,
		"third", "fourth",
	}, "\n")
	sr := retrieve.ScoredResult{
		QualifiedName: "pkg.Fn",
		Kind:          "function",
		File:          "pkg/fn.go",
		StartLine:     100,
		EndLine:       241,
		Detail:        "excerpt",
		Body:          body,
		BodyStartLine: 100,
		BodyEndLine:   241,
		BodySegments: []retrieve.BodySegment{
			{StartLine: 100, Lines: 2},
			{StartLine: 240, Lines: 2},
		},
	}

	md := renderMarkdown("req-1", retrieve.OutputModeAnswer, retrieve.Result{
		Symbols: []retrieve.ScoredResult{sr},
	})

	require.Contains(t, md, "100\tfirst")
	require.Contains(t, md, "101\tsecond")
	require.Contains(t, md, "240\tthird",
		"the line after the gap marker belongs to the second window")
	require.Contains(t, md, "241\tfourth")
	require.NotContains(t, md, "102\tthird",
		"numbering straight through the gap cites a line the code is not on")
}
