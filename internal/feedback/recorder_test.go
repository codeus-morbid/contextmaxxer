package feedback

import (
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
