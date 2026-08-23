package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A misclassified section is worse than no measurement: it moves tokens into
// "everything else" and hides exactly the line worth cutting. This sample is
// copied from a real cockroach response.
func TestBreakdownClassifiesEverySection(t *testing.T) {
	md := strings.Join([]string{
		"req:5b044cd5 (pass to record_feedback)",
		"",
		"1. **A1m1f** (function) pkg/geo/geodesic.c:1605-1613 [medium]",
		"```",
		"1605\treal A1m1f(real eps)  {",
		"1606\t  return 0;",
		"```",
		"callers (path_status=static_unverified; verify branch/dispatch): geod_lineinit_int (pkg/geo/geodesic.c:414-499@465) [callsite: 463 if (x) { | 464 real s;]",
		"callees (path_status=static_unverified; verify branch/dispatch): polyval (pkg/geo/geodesic.c:219-223@1611)",
		"",
		"2. **B** (function) pkg/b.go:1-2 [low] — func B() error // does things",
		"note: low retrieval confidence — rephrase naming the mechanism",
	}, "\n")

	got := breakdown(md)

	require.NotZero(t, got[secGraph], "graph lines must not fall into the catch-all")
	require.NotZero(t, got[secGraphBoil], "the repeated disclaimer must be priced on its own")
	require.NotZero(t, got[secCode])
	require.NotZero(t, got[secLineNums])
	require.NotZero(t, got[secHeader])
	require.NotZero(t, got[secTeaser])
	require.NotZero(t, got[secNote])
	require.NotZero(t, got[secRequestID])

	// The catch-all should hold only fences and blank lines here.
	require.LessOrEqual(t, got[secFraming], 4,
		"unclassified text is hiding somewhere: %d tokens", got[secFraming])
}
