package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServer_Constructs(t *testing.T) {
	// DECISION: Retriever has no interface used by Server — test that construction
	// succeeds with a nil retriever (Serve is not called in this test).
	var r *retrieve.Retriever
	srv := NewServer(r, slog.Default())
	assert.NotNil(t, srv)
}

func TestFindContextToolContractPreservesCompactDefaults(t *testing.T) {
	for _, fragment := range []string{
		"compact tail entries are candidates only",
		"LARGE codebase you do not already know",
		"use grep instead",
		"static candidates, not proof",
		"path_status=static_unverified",
		"verify the branch, feature flag, protocol, or dispatch discriminator",
		"call expand_context",
		"call continue_context",
		"until status:complete",
		"without rerunning semantic search",
		// Query shape is the largest measured lever the caller controls: on 493
		// SWE-Explore tasks a title beat the full issue report by 17% precision,
		// more than the graph and the cross-encoder are worth combined.
		"one sentence naming the mechanism",
	} {
		assert.True(t, strings.Contains(findContextToolDescription, fragment), "tool description missing %q", fragment)
	}
	assert.NotContains(t, findContextToolDescription, "full_bodies")
}

func TestCachedRankReturnsOneRank(t *testing.T) {
	srv := NewServer(nil, slog.Default())
	srv.cacheExpansion("req-1", []retrieve.ScoredResult{
		{QualifiedName: "pkg.A", StartLine: 10, EndLine: 20, Body: "func A() {}"},
		{QualifiedName: "pkg.B", StartLine: 30, EndLine: 45, Body: "func B() {}"},
	})

	got, err := srv.cachedRank("req-1", 2)
	require.NoError(t, err)
	assert.Equal(t, "pkg.B", got.QualifiedName)
}

func TestCachedRankSurvivesLongSessionAndRejectsInvalidRanks(t *testing.T) {
	srv := NewServer(nil, slog.Default())
	const requestCount = 256
	for i := 0; i < requestCount; i++ {
		srv.cacheExpansion(fmt.Sprintf("req-%d", i), []retrieve.ScoredResult{{QualifiedName: "pkg.A"}})
	}
	_, err := srv.cachedRank("req-0", 1)
	require.NoError(t, err)
	_, err = srv.cachedRank(fmt.Sprintf("req-%d", requestCount-1), 2)
	assert.ErrorContains(t, err, "out of range")
}

func TestExpansionPagesAreLosslessAndExplicit(t *testing.T) {
	body := "func Huge() {\n" + strings.Repeat("\tuse(\"ёж\")\n", 6000) + "}\n"
	srv := NewServer(nil, slog.Default())
	state := continuationState{
		RequestID: "req-1",
		Rank:      1,
		Symbol: retrieve.ScoredResult{
			QualifiedName: "pkg.Huge", Kind: "function", File: "pkg/huge.go", StartLine: 10, EndLine: 6011,
		},
		Body: body, SHA256: "digest",
	}

	var rebuilt strings.Builder
	page := srv.pageFromState(state)
	for {
		rebuilt.WriteString(page.Body)
		out := renderExpansionPage(page)
		if page.Complete() {
			assert.Contains(t, out, "status:complete")
			assert.NotContains(t, out, "NEXT ACTION REQUIRED")
			break
		}
		assert.Contains(t, out, "status:more")
		assert.Contains(t, out, "NEXT ACTION REQUIRED")
		require.NotEmpty(t, page.NextCursor)
		var err error
		page, err = srv.continueExpansion(page.NextCursor)
		require.NoError(t, err)
	}
	assert.Equal(t, body, rebuilt.String())
}

func TestExpansionPageHandlesOneHugeUTF8Line(t *testing.T) {
	body := strings.Repeat("ёж", expansionPageBytes)
	srv := NewServer(nil, slog.Default())
	page := srv.pageFromState(continuationState{Body: body, SHA256: "digest", Symbol: retrieve.ScoredResult{StartLine: 7}})
	require.False(t, page.Complete())
	assert.True(t, utf8.ValidString(page.Body))
	assert.Equal(t, 7, page.StartLine)
	assert.Greater(t, page.EndColumn, page.StartColumn)
}

func TestRenderMarkdownKeepsGraphOnCompactAndMarksExcerpt(t *testing.T) {
	out := renderMarkdown("req-1", retrieve.OutputModeAnswer, retrieve.Result{Symbols: []retrieve.ScoredResult{
		{
			QualifiedName: "pkg.Compact", Kind: "function", File: "pkg/a.go", StartLine: 10, EndLine: 20,
			Body: "func Compact()", Detail: "compact",
			Callees: []retrieve.SymbolRef{{
				QualifiedName: "pkg.Next", File: "pkg/b.go", Lines: "30-40", CallLine: 17,
				PathStatus: "static_unverified", CallSite: "16 case enabled: | 17 Next()",
			}},
		},
		{
			QualifiedName: "pkg.Excerpt", Kind: "function", File: "pkg/c.go", StartLine: 100, EndLine: 180,
			Body: "callNext()", Detail: "excerpt", BodyStartLine: 140, BodyEndLine: 140,
		},
	}})
	assert.Contains(t, out, "callees: pkg.Next (pkg/b.go:30-40@17)")
	assert.Contains(t, out, "[callsite: 16 case enabled: | 17 Next()]")
	// The marker names what is missing, not just what is shown. A question that
	// needs every branch of a function is answered wrongly from a window, and
	// the agent cannot know to expand unless the loss is on the page.
	assert.Contains(t, out, "[excerpt 140-140 — 1 of 81 lines; expand rank 2 for the rest]")

	// The caveat is stated once, not appended to every graph line: repeated it
	// measured at 84 of 1028 response tokens on cockroach (cmd/rspbreak).
	assert.Equal(t, 1, strings.Count(out, "path_status=static_unverified"),
		"the static-edge caveat belongs in one legend line")
}

func TestRenderMarkdownOmitsTheCaveatWhenNoGraphRefs(t *testing.T) {
	out := renderMarkdown("req-1", retrieve.OutputModeAnswer, retrieve.Result{Symbols: []retrieve.ScoredResult{{
		QualifiedName: "pkg.Lonely", Kind: "function", File: "pkg/a.go", StartLine: 1, EndLine: 2,
		Body: "func Lonely()", Detail: "full",
	}}})

	assert.NotContains(t, out, "path_status", "nothing to caveat, so no caveat")
}

func TestSymbolRefOutputsExposeStaticPathStatusAndCallSite(t *testing.T) {
	data, err := json.Marshal(symbolRefOutputs([]retrieve.SymbolRef{{
		QualifiedName: "pkg.Next", File: "pkg/b.go", Lines: "30-40", Kind: "function", CallLine: 17,
		PathStatus: "static_unverified", CallSite: "16 case enabled: | 17 Next()",
	}}))
	require.NoError(t, err)
	assert.JSONEq(t, `[{"name":"pkg.Next","file":"pkg/b.go","lines":"30-40","kind":"function","call_line":17,"path_status":"static_unverified","callsite_evidence":"16 case enabled: | 17 Next()"}]`, string(data))
}

func TestServerRecordsFeedback(t *testing.T) {
	rec := feedback.NewRecorder(filepath.Join(t.TempDir(), "feedback.jsonl"))
	srv := NewServerWithFeedback(nil, rec, slog.Default())

	require.NoError(t, srv.recordFeedback(context.Background(), feedback.FeedbackEvent{
		RequestID:       "req-1",
		Query:           "find context",
		SelectedSymbols: []string{"pkg.Target"},
		Outcome:         "used",
		Source:          "test",
	}))
}

// The agent cites these numbers, so a wrong one is worse than none. Numbering
// straight through a gap marker would misattribute every line after the gap.
func TestNumberBodyJumpsAtEachGap(t *testing.T) {
	body := strings.Join([]string{
		"first", "second",
		retrieve.EvidenceGapMarker,
		"third", "fourth",
		retrieve.EvidenceGapMarker,
		"fifth",
	}, "\n")
	segments := []retrieve.BodySegment{
		{StartLine: 100, Lines: 2},
		{StartLine: 240, Lines: 2},
		{StartLine: 900, Lines: 1},
	}

	got := numberBody(body, 100, segments)

	require.Equal(t, strings.Join([]string{
		"100\tfirst", "101\tsecond",
		retrieve.EvidenceGapMarker,
		"240\tthird", "241\tfourth",
		retrieve.EvidenceGapMarker,
		"900\tfifth",
	}, "\n"), got)
}

func TestNumberBodyWithoutSegmentsIsSequential(t *testing.T) {
	require.Equal(t, "7\ta\n8\tb", numberBody("a\nb", 7, nil))
}

func TestVisibleSpanTextNamesEveryWindow(t *testing.T) {
	// One range for a single window; one per window otherwise — reporting only
	// first-to-last would claim the gaps are visible.
	require.Equal(t, "10-20", visibleSpanText(retrieve.ScoredResult{
		BodyStartLine: 10, BodyEndLine: 20,
	}))
	require.Equal(t, "10-11,40-42", visibleSpanText(retrieve.ScoredResult{
		BodyStartLine: 10, BodyEndLine: 42,
		BodySegments: []retrieve.BodySegment{{StartLine: 10, Lines: 2}, {StartLine: 40, Lines: 3}},
	}))
}

// The tuning parameters are 42% of find_context's schema — 1,071 characters of
// 2,563 — and the tool's own description tells an agent not to use them. They
// are paid for on every session whether or not they are called, so they are off
// the schema unless a harness asks for them.
func TestExperimentalOptionsAreOffByDefault(t *testing.T) {
	t.Setenv(experimentalFindContextTuning, "")
	assert.Empty(t, experimentalFindContextOptions())

	t.Setenv(experimentalFindContextTuning, "1")
	assert.Len(t, experimentalFindContextOptions(), 8,
		"the ablation harnesses in cmd/ still need every knob")
}
