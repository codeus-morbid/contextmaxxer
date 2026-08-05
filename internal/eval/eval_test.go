package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/stretchr/testify/require"
)

func TestSummarizeIncludesHitAt1And3(t *testing.T) {
	results := []Result{
		{RecallAt5: true, RecallAt10: true, MRR: 1.0, Hits: []int{1}},
		{RecallAt5: true, RecallAt10: true, MRR: 1.0 / 3.0, Hits: []int{3}},
		{RecallAt5: false, RecallAt10: true, MRR: 1.0 / 8.0, Hits: []int{8}},
	}

	s := SummarizeDetailed(results)

	require.InDelta(t, 1.0/3.0, s.HitAt1, 1e-9)
	require.InDelta(t, 2.0/3.0, s.HitAt3, 1e-9)
	require.InDelta(t, 2.0/3.0, s.RecallAt5, 1e-9)
	require.InDelta(t, 1.0, s.RecallAt10, 1e-9)
}

func TestDumpRankingDiagnosticsWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ranking.jsonl")
	results := []Result{{
		QueryID:  "q1",
		Mode:     "hybrid",
		Expected: []string{"pkg.Target"},
		Symbols: []retrieve.ScoredResult{{
			File:          "pkg/file.go",
			QualifiedName: "pkg.Target",
			Kind:          "function",
			Score:         0.9,
			Why:           "feature_rank",
			Features:      retrieve.RankingFeatures{NameOverlap: 1, EffectiveAlpha: 0.9, SeedRank: 1, PPRRank: 3, SeedScore: 1, PPRScore: 0.5},
		}},
		Stats: retrieve.Stats{EffectiveAlpha: 0.9},
	}}

	require.NoError(t, DumpRankingDiagnostics(path, results))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), `"query_id":"q1"`)
	require.Contains(t, string(data), `"qualified_name":"pkg.Target"`)
	require.Contains(t, string(data), `"hit":true`)
	require.Contains(t, string(data), `"NameOverlap":1`)
	require.Contains(t, string(data), `"effective_alpha":0.9`)
	require.Contains(t, string(data), `"EffectiveAlpha":0.9`)
	require.Contains(t, string(data), `"SeedRank":1`)
	require.Contains(t, string(data), `"PPRRank":3`)
}

func TestLoadManifestResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	content := `{
  "projects": [
    {
      "name": "sample",
      "index_path": "../sample/.contextmaxxer/index.db",
      "cases_path": "./sample_queries.json"
    }
  ]
}`
	require.NoError(t, os.WriteFile(manifestPath, []byte(content), 0644))

	m, err := LoadManifest(manifestPath)
	require.NoError(t, err)
	require.Len(t, m.Projects, 1)
	require.Equal(t, "sample", m.Projects[0].Name)
	require.True(t, filepath.IsAbs(m.Projects[0].IndexPath))
	require.True(t, filepath.IsAbs(m.Projects[0].CasesPath))
}

func TestSummarizeGlobalWeightedByQueryCount(t *testing.T) {
	s := SummarizeGlobal([]ProjectSummary{
		{
			Project:    "a",
			QueryCount: 1,
			Vector:     Summary{HitAt1: 0.0},
			Hybrid:     Summary{HitAt1: 1.0},
			AvgLatency: 100 * time.Millisecond,
		},
		{
			Project:    "b",
			QueryCount: 3,
			Vector:     Summary{HitAt1: 1.0},
			Hybrid:     Summary{HitAt1: 0.0},
			AvgLatency: 100 * time.Millisecond,
		},
	})
	require.Equal(t, 4, s.QueryCount)
	require.InDelta(t, 0.75, s.Vector.HitAt1, 1e-9)
	require.InDelta(t, 0.25, s.Hybrid.HitAt1, 1e-9)
}
