package retrieve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func results(files []string, scores []float32, rel0 float32) []ScoredResult {
	out := make([]ScoredResult, len(files))
	for i := range files {
		out[i] = ScoredResult{File: files[i], Score: scores[i]}
	}
	out[0].Relevance = rel0
	return out
}

// Each case is the shape that the old gap/tie rule read the wrong way round.
// Over 848 SWE-Explore instances a lone leader was the least reliable answer
// and a cluster from one file the most reliable one.
func TestRetrievalHealthReadsConsensusNotLead(t *testing.T) {
	cases := []struct {
		name   string
		files  []string
		scores []float32
		rel0   float32
		want   string
	}{
		{
			// All five tied, three in one file. The old rule: gap 0, five
			// ties -> "low". Rank 1 is right 85-94% of the time here.
			name:   "tied cluster from one file",
			files:  []string{"a.go", "a.go", "a.go", "b.go", "c.go"},
			scores: []float32{1.00, 0.99, 0.98, 0.97, 0.96},
			rel0:   0.85,
			want:   "high",
		},
		{
			// Rank 1 far ahead of four other files. The old rule: gap 0.5,
			// one tie -> "high". Rank 1 is right 34-43% of the time here.
			name:   "lone leader",
			files:  []string{"a.go", "b.go", "c.go", "d.go", "e.go"},
			scores: []float32{1.00, 0.50, 0.40, 0.30, 0.20},
			rel0:   0.85,
			want:   "low",
		},
		{
			name:   "two from rank 1's file",
			files:  []string{"a.go", "b.go", "a.go", "c.go", "d.go"},
			scores: []float32{1.00, 0.90, 0.80, 0.70, 0.60},
			rel0:   0.85,
			want:   "medium",
		},
		{
			// Full agreement, but the cross-encoder says rank 1 does not
			// answer the query: the absent-concept floor still wins.
			name:   "agreement below the relevance floor",
			files:  []string{"a.go", "a.go", "a.go", "a.go", "a.go"},
			scores: []float32{1.00, 0.99, 0.98, 0.97, 0.96},
			rel0:   0.50,
			want:   "low",
		},
		{
			// Relevance 0 means the reranker never ran; no verdict is not a
			// weak verdict, so the floor stays out of it.
			name:   "no rerank verdict",
			files:  []string{"a.go", "a.go", "a.go", "b.go", "c.go"},
			scores: []float32{1.00, 0.99, 0.98, 0.97, 0.96},
			rel0:   0,
			want:   "high",
		},
		{
			// A position lookup or a one-result answer: nothing disagrees.
			name:   "single result",
			files:  []string{"a.go"},
			scores: []float32{1.00},
			rel0:   0.85,
			want:   "high",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := buildRetrievalHealth(results(tc.files, tc.scores, tc.rel0), len(tc.files))
			require.NotNil(t, h)
			assert.Equal(t, tc.want, h.Confidence)
			if tc.want == "low" {
				assert.NotEmpty(t, h.Suggestion, "a low label has to say what to do")
			}
		})
	}
}

func TestRetrievalHealthCountsConsensusOverTopFive(t *testing.T) {
	// The sixth result shares rank 1's file but is outside the window.
	h := buildRetrievalHealth(results(
		[]string{"a.go", "b.go", "c.go", "d.go", "e.go", "a.go"},
		[]float32{1, 0.9, 0.8, 0.7, 0.6, 0.5}, 0.85), 6)
	assert.Equal(t, 1, h.Consensus)
	assert.Equal(t, "low", h.Confidence)
}
