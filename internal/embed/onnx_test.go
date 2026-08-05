package embed

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestEmbedder(t *testing.T) *OnnxEmbedder {
	t.Helper()
	if !modelIntegrationTestsEnabled() {
		t.Skip("skipping model integration test (set CONTEXTMAXXER_RUN_MODEL_TESTS=1 to run)")
	}

	// DECISION(2026-08): model-backed tests are opt-in so the default unit suite
	// stays hermetic. ASSUMES: CI without the opt-in must never download model
	// artifacts. REVISIT IF: a small deterministic model fixture is checked in.
	cacheDir := "../../testdata/cache"
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("create test cache dir: %v", err)
	}

	e, err := NewOnnxEmbedder(context.Background(), Config{
		ModelName: "bge-small-en-v1.5",
		CacheDir:  cacheDir,
		BatchSize: 8,
	})
	require.NoError(t, err)
	t.Cleanup(func() { e.Close() })
	return e
}

func modelIntegrationTestsEnabled() bool {
	return os.Getenv("CONTEXTMAXXER_RUN_MODEL_TESTS") == "1" &&
		os.Getenv("CONTEXTMAXXER_SKIP_DOWNLOAD") == ""
}

func TestModelIntegrationTestsEnabled(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_RUN_MODEL_TESTS", "")
	t.Setenv("CONTEXTMAXXER_SKIP_DOWNLOAD", "")
	require.False(t, modelIntegrationTestsEnabled(), "model tests must be opt-in")

	t.Setenv("CONTEXTMAXXER_RUN_MODEL_TESTS", "1")
	require.True(t, modelIntegrationTestsEnabled())

	t.Setenv("CONTEXTMAXXER_SKIP_DOWNLOAD", "1")
	require.False(t, modelIntegrationTestsEnabled(), "skip-download must override the opt-in")
}

func TestOnnxEmbedder_DimensionMatches(t *testing.T) {
	e := newTestEmbedder(t)
	require.Equal(t, 384, e.Dimension())
}

func TestOnnxEmbedder_EmbedShape(t *testing.T) {
	e := newTestEmbedder(t)

	texts := []string{
		"the quick brown fox",
		"jumps over the lazy dog",
		"hello world",
	}
	vecs, err := e.Embed(context.Background(), texts)
	require.NoError(t, err)
	require.Len(t, vecs, 3)

	for i, v := range vecs {
		require.Len(t, v, 384, "row %d length", i)
		var nonZero bool
		for _, x := range v {
			if x != 0 {
				nonZero = true
				break
			}
		}
		require.True(t, nonZero, "row %d is all zeros", i)
	}
}

func TestOnnxEmbedder_SimilarTextsCloser(t *testing.T) {
	e := newTestEmbedder(t)

	texts := []string{
		"dog runs in park",
		"puppy plays in field",
		"compile error in golang",
	}
	vecs, err := e.Embed(context.Background(), texts)
	require.NoError(t, err)

	cos01 := cosine(vecs[0], vecs[1])
	cos02 := cosine(vecs[0], vecs[2])
	t.Logf("cosine(dog,puppy)=%.4f cosine(dog,golang)=%.4f", cos01, cos02)
	require.Greater(t, cos01, cos02, "semantically similar texts should be closer")
}

func TestOnnxEmbedder_Normalized(t *testing.T) {
	e := newTestEmbedder(t)

	vecs, err := e.Embed(context.Background(), []string{"normalization test"})
	require.NoError(t, err)
	require.Len(t, vecs, 1)

	norm := l2Norm(vecs[0])
	t.Logf("L2 norm = %.6f", norm)
	require.InDelta(t, 1.0, norm, 1e-3)
}

func TestOnnxEmbedder_Batching(t *testing.T) {
	e := newTestEmbedder(t)

	texts := make([]string, 50)
	for i := range texts {
		texts[i] = "test sentence number"
	}

	vecs, err := e.Embed(context.Background(), texts)
	require.NoError(t, err)
	require.Len(t, vecs, 50)
	for i, v := range vecs {
		require.Len(t, v, 384, "row %d", i)
	}
}

func TestOnnxEmbedder_Jina_DimensionAndNorm(t *testing.T) {
	if testing.Short() || os.Getenv("CONTEXTMAXXER_FAST_TESTS") != "" || os.Getenv("CONTEXTMAXXER_RUN_LARGE_MODEL_TESTS") == "" {
		t.Skip("skipping jina model test (set CONTEXTMAXXER_RUN_LARGE_MODEL_TESTS=1 to run)")
	}
	if os.Getenv("CONTEXTMAXXER_SKIP_DOWNLOAD") != "" {
		t.Skip("skipping download-required test")
	}

	cacheDir := "../../testdata/cache"
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		t.Fatalf("create test cache dir: %v", err)
	}

	e, err := NewOnnxEmbedder(context.Background(), Config{
		ModelName: "jina-embeddings-v2-base-code",
		CacheDir:  cacheDir,
		BatchSize: 4,
	})
	require.NoError(t, err)
	defer e.Close()

	require.Equal(t, 768, e.Dimension())

	vecs, err := e.Embed(context.Background(), []string{"func main() { fmt.Println(\"hello\") }"})
	require.NoError(t, err)
	require.Len(t, vecs, 1)
	require.Len(t, vecs[0], 768)

	norm := l2Norm(vecs[0])
	require.InDelta(t, 1.0, norm, 1e-3, "output should be L2-normalized")
}

func TestModelSpecs_InputNamesMatchONNXInputs(t *testing.T) {
	bge, err := GetModel("bge-small-en-v1.5")
	require.NoError(t, err)
	require.Equal(t, []string{"input_ids", "attention_mask", "token_type_ids"}, bge.InputNames)

	jina, err := GetModel("jina-embeddings-v2-base-code")
	require.NoError(t, err)
	require.Equal(t, []string{"input_ids", "attention_mask"}, jina.InputNames)
}

func cosine(a, b []float32) float32 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom < 1e-9 {
		return 0
	}
	return float32(dot / denom)
}

func l2Norm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}
