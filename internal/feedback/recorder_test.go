package feedback

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecorderWritesRetrievalAndFeedbackEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.jsonl")
	rec := NewRecorder(path)

	require.NoError(t, rec.RecordRetrieval(RetrievalEvent{
		RequestID: "req-1",
		Query:     "find sqlite vector search",
		Candidates: []Candidate{{
			Rank:          1,
			File:          "internal/store/sqlite/sqlite.go",
			QualifiedName: "sqlite.Store.SearchByVectorScored",
			Kind:          "method",
			Score:         0.95,
			Why:           "hybrid_seed+rerank",
		}},
	}))
	require.NoError(t, rec.RecordFeedback(FeedbackEvent{
		RequestID:       "req-1",
		Query:           "find sqlite vector search",
		SelectedSymbols: []string{"sqlite.Store.SearchByVectorScored"},
		Outcome:         "used",
		Source:          "mcp",
	}))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := splitJSONLines(string(data))
	require.Len(t, lines, 2)

	var first map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.Equal(t, "retrieval", first["event"])
	require.Equal(t, "req-1", first["request_id"])

	var second map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))
	require.Equal(t, "feedback", second["event"])
	require.Equal(t, "used", second["outcome"])
}

func TestNewRequestIDIsNotEmpty(t *testing.T) {
	require.NotEmpty(t, NewRequestID())
	require.NotEqual(t, NewRequestID(), NewRequestID())
}

func splitJSONLines(data string) []string {
	var lines []string
	for _, line := range strings.Split(data, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
