package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFixtureRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "qrels"), 0755))

	corpus := []string{
		`{"_id": "c1", "text": "func retryBackoff() { sleep and retry the request }"}`,
		`{"_id": "c2", "text": "func flushCache() { drop all cached entries }"}`,
		`{"_id": "c3", "text": "func retryLimit() { cap the number of retries }"}`,
		`{"_id": "c4", "text": "func parseConfig() { read yaml settings }"}`,
		`{"_id": "c5", "text": "func retryJitter() { add jitter to retry delay }"}`,
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "corpus.jsonl"), []byte(strings.Join(corpus, "\n")), 0644))

	// q1 restricted by temporal filter to c1,c3,c4 (c5 must never appear).
	queries := []string{
		`{"_id": "q1", "text": "retries fail forever", "filtered_corpus_id": ["c1", "c3", "c4"]}`,
		`{"_id": "q2", "text": "no relevant docs for this one"}`,
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "queries.jsonl"), []byte(strings.Join(queries, "\n")), 0644))

	qrels := "query-id\tcorpus-id\tscore\nq1\tc1\t1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qrels", "test.tsv"), []byte(qrels), 0644))
}

func TestEmitRepo_TripletRules(t *testing.T) {
	dir := t.TempDir()
	writeFixtureRepo(t, dir)

	out := filepath.Join(t.TempDir(), "train.jsonl")
	f, err := os.Create(out)
	require.NoError(t, err)
	enc := json.NewEncoder(f)

	emitted, skipped, err := emitRepo(enc, dir, "fixture/repo", 8, 4, 4000)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, 1, emitted, "q1 emits, q2 has no qrels")
	require.Equal(t, 1, skipped)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	var tr triplet
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(data))), &tr))

	require.Equal(t, "retries fail forever", tr.Query)
	require.Equal(t, []string{"func retryBackoff() { sleep and retry the request }"}, tr.Pos)
	// Negatives: only from the temporal filter (c3, c4), never the positive
	// (c1) and never outside the filter (c2, c5).
	require.NotEmpty(t, tr.Neg)
	for _, n := range tr.Neg {
		require.NotContains(t, n, "retryBackoff", "positive leaked into negatives")
		require.NotContains(t, n, "flushCache", "outside temporal filter")
		require.NotContains(t, n, "retryJitter", "outside temporal filter")
	}
	// BM25 should put the retry-y c3 above the unrelated c4.
	require.Contains(t, tr.Neg[0], "retryLimit", "hard negative should be the lexically-closest non-relevant chunk")
}

func TestIsHoldout_DeterministicAndForced(t *testing.T) {
	forced := map[string]bool{"special__repo": true}
	require.True(t, isHoldout("ds/special__repo", "special__repo", forced, 0), "forced list wins even at pct=0")

	// Deterministic: same key, same verdict, any number of calls.
	a := isHoldout("ds/some__repo", "some__repo", nil, 15)
	for i := 0; i < 10; i++ {
		require.Equal(t, a, isHoldout("ds/some__repo", "some__repo", nil, 15))
	}
	// pct=0 keeps everything in train; pct=100 holds everything out.
	require.False(t, isHoldout("ds/x", "x", nil, 0))
	require.True(t, isHoldout("ds/x", "x", nil, 100))
}

func TestTrainJSONLIsOneObjectPerLine(t *testing.T) {
	dir := t.TempDir()
	writeFixtureRepo(t, dir)
	out := filepath.Join(t.TempDir(), "train.jsonl")
	f, _ := os.Create(out)
	_, _, err := emitRepo(json.NewEncoder(f), dir, "fixture/repo", 2, 4, 4000)
	require.NoError(t, err)
	f.Close()

	rf, _ := os.Open(out)
	defer rf.Close()
	sc := bufio.NewScanner(rf)
	lines := 0
	for sc.Scan() {
		var tr triplet
		require.NoError(t, json.Unmarshal(sc.Bytes(), &tr))
		lines++
	}
	require.Equal(t, 1, lines)
}
