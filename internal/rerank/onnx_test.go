package rerank

import (
	"strings"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildXLMRobertaPairIDs_InsertsDoubleSeparator(t *testing.T) {
	query := []uint32{0, 11, 12, 2}
	doc := []uint32{0, 21, 22, 2}

	got := buildXLMRobertaPairIDs(query, doc, 16, 8)

	assert.Equal(t, []int64{0, 11, 12, 2, 2, 21, 22, 2}, got)
}

func TestBuildXLMRobertaPairIDs_TruncatesDocumentFirst(t *testing.T) {
	query := []uint32{0, 11, 12, 2}
	doc := []uint32{0, 21, 22, 23, 24, 25, 2}

	got := buildXLMRobertaPairIDs(query, doc, 8, 4)

	assert.Equal(t, []int64{0, 11, 12, 2, 2, 21, 22, 2}, got)
}

func TestCandidateTextTruncatesLargeBodies(t *testing.T) {
	body := strings.Repeat("x", 5000)

	got := candidateText(retrieve.ScoredResult{
		File:          "internal/api/server.go",
		QualifiedName: "api.NewServer",
		Kind:          "function",
		Body:          body,
	})

	require.Contains(t, got, "file: internal/api/server.go")
	require.Contains(t, got, "symbol: api.NewServer")
	require.Less(t, len(got), 2200)
	require.NotContains(t, got, strings.Repeat("x", 2500))
}

func TestCandidateTextUsesStructuredFieldsBeforeBody(t *testing.T) {
	got := candidateText(retrieve.ScoredResult{
		File:          "internal/rerank/onnx.go",
		QualifiedName: "rerank.OnnxReranker.scoreBatch",
		Kind:          "method",
		Signature:     "func (r *OnnxReranker) scoreBatch(ctx context.Context, query string, docs []string) ([]float32, error)",
		Docstring:     "scoreBatch scores many query-document pairs.",
		Body:          "func (r *OnnxReranker) scoreBatch(...) { return nil, nil }",
	})

	require.Contains(t, got, "symbol: rerank.OnnxReranker.scoreBatch\n")
	require.Contains(t, got, "kind: method\n")
	require.Contains(t, got, "file: internal/rerank/onnx.go\n")
	require.Contains(t, got, "signature: func (r *OnnxReranker) scoreBatch")
	require.Contains(t, got, "summary: scoreBatch scores many query-document pairs.\n")
	require.Contains(t, got, "body:\nfunc (r *OnnxReranker) scoreBatch")
	require.Less(t, strings.Index(got, "signature:"), strings.Index(got, "body:"))
}
