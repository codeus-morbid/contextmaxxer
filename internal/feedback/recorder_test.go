package feedback

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecorder_RotatesAtTheCap(t *testing.T) {
	// Without a cap this file grew to 538MB over two and a half months of
	// dogfooding — seventy times the index it describes — holding every query
	// string since the beginning. The cap bounds the pair at 2x and keeps one
	// previous generation so an export still has history to read.
	dir := t.TempDir()
	path := filepath.Join(dir, "feedback.jsonl")
	r := NewRecorder(path)
	require.NotNil(t, r)
	r.maxBytes = 4 << 10 // small enough to rotate within the loop below
	// Rotation is a property of the append path, so drive it directly rather
	// than through the label gate that decides WHICH events get appended.
	r.logAll = true

	for i := 0; i < 200; i++ {
		require.NoError(t, r.RecordRetrieval(RetrievalEvent{
			Query:      strings.Repeat("where is the ranking computed ", 4),
			Candidates: []Candidate{{Rank: 1, QualifiedName: "pkg.Fn", File: "a.go"}},
		}))
	}

	cur, err := os.Stat(path)
	require.NoError(t, err)
	rot, err := os.Stat(RotatedPath(path))
	require.NoError(t, err, "one previous generation must be kept for export to read")

	require.Less(t, cur.Size(), int64(2*r.maxBytes),
		"the live file must stay near the cap, not grow without bound")
	require.Less(t, rot.Size(), int64(2*r.maxBytes))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "exactly two generations: the live log and one rotation")
}

func TestRecorder_DefaultCapIsSet(t *testing.T) {
	r := NewRecorder(filepath.Join(t.TempDir(), "f.jsonl"))
	require.Equal(t, int64(defaultMaxLogBytes), r.maxBytes,
		"a recorder with no override must still be capped")
}

func linesIn(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestRecorder_WritesOnlyLabelledRetrievals(t *testing.T) {
	// 95958 retrievals against 57 labels is what writing everything produced:
	// inputs with no outputs, which cannot train the ranker the log exists for.
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	r := NewRecorder(path)

	for i := 0; i < 50; i++ {
		require.NoError(t, r.RecordRetrieval(RetrievalEvent{
			RequestID: fmt.Sprintf("req-%d", i),
			Query:     "where is ranking computed",
			Candidates: []Candidate{{Rank: 1, QualifiedName: "pkg.Fn",
				Features: map[string]float32{"ppr": 0.5}}},
		}))
	}
	require.Empty(t, linesIn(t, path), "unlabelled retrievals must not reach disk")

	require.NoError(t, r.RecordFeedback(FeedbackEvent{
		RequestID:       "req-7",
		SelectedSymbols: []string{"pkg.Fn"},
	}))

	lines := linesIn(t, path)
	require.Len(t, lines, 2, "the labelled retrieval and its label, and nothing else")
	require.Contains(t, lines[0], `"event":"retrieval"`, "candidates come before the label that judges them")
	require.Contains(t, lines[0], "req-7")
	require.Contains(t, lines[0], `"ppr"`, "the feature vector must survive the wait")
	require.Contains(t, lines[1], `"event":"feedback"`)
}

func TestRecorder_LabelSurvivesAnAgedOutRetrieval(t *testing.T) {
	// The label is the scarce half: if its retrieval has fallen out of the ring
	// it is still worth writing, just without features.
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	r := NewRecorder(path)

	require.NoError(t, r.RecordRetrieval(RetrievalEvent{RequestID: "old"}))
	for i := 0; i < pendingRetrievals+5; i++ {
		require.NoError(t, r.RecordRetrieval(RetrievalEvent{RequestID: fmt.Sprintf("r%d", i)}))
	}
	require.NoError(t, r.RecordFeedback(FeedbackEvent{RequestID: "old", Outcome: "used"}))

	lines := linesIn(t, path)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], `"event":"feedback"`)
}

func TestRecorder_FeedbackAllRestoresTheFirehose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	r := NewRecorder(path)
	r.logAll = true

	require.NoError(t, r.RecordRetrieval(RetrievalEvent{RequestID: "a"}))
	require.NoError(t, r.RecordRetrieval(RetrievalEvent{RequestID: "b"}))
	require.Len(t, linesIn(t, path), 2, "debugging mode writes every served response")
}
