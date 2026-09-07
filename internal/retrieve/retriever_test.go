package retrieve

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockStore struct {
	symbols    []store.Symbol
	edges      []store.Edge
	ids        []int64
	filePaths  map[int64]string
	ftsResult  []store.ScoredSymbol
	ftsByQuery map[string][]store.ScoredSymbol
	// onSearchByText lets a test observe WHAT was asked, not just what came back.
	onSearchByText func(query string)
	// vecResult pins what the vector channel returns. Without it the stub hands
	// back every symbol, which makes "unreachable except through channel X"
	// untestable — the vector channel would have found it anyway.
	vecResult      []store.ScoredSymbol
	bodyFTSResult  []store.ScoredSymbol
	chunkVecResult []store.ScoredSymbol
	// metaCache mirrors production, where ListSymbolMeta is served from a cache
	// and hands back the SAME backing array every call. The graph memo keys its
	// freshness on slice identity, so a mock that rebuilds the slice silently
	// disables the cache and hides every bug that lives in shared memo state.
	metaCache []store.Symbol
}

func (m *mockStore) SearchByVectorScored(_ context.Context, _ []float32, k int) ([]store.ScoredSymbol, error) {
	if m.vecResult != nil {
		return m.vecResult, nil
	}
	var out []store.ScoredSymbol
	for i, sym := range m.symbols {
		if i >= k {
			break
		}
		score := float32(1.0) / float32(i+1)
		out = append(out, store.ScoredSymbol{Symbol: sym, Score: score})
	}
	return out, nil
}

func (m *mockStore) GetSymbolsByIDs(_ context.Context, ids []int64) ([]store.Symbol, error) {
	idx := make(map[int64]store.Symbol, len(m.symbols))
	for _, s := range m.symbols {
		idx[s.ID] = s
	}
	var out []store.Symbol
	for _, id := range ids {
		if s, ok := idx[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *mockStore) GetSymbolBody(_ context.Context, symbolID int64) (store.SymbolBody, error) {
	for _, symbol := range m.symbols {
		if symbol.ID != symbolID {
			continue
		}
		body := symbol.BodyExcerpt
		if symbol.FullBody != "" {
			body = symbol.FullBody
		}
		return store.SymbolBody{SymbolID: symbolID, Body: body, SHA256: "test"}, nil
	}
	return store.SymbolBody{}, store.ErrNotFound
}

func (m *mockStore) GetFilesByIDs(_ context.Context, ids []int64) (map[int64]string, error) {
	result := make(map[int64]string, len(ids))
	for _, id := range ids {
		if p, ok := m.filePaths[id]; ok {
			result[id] = p
		}
	}
	return result, nil
}

func (m *mockStore) ListAllSymbolIDs(_ context.Context) ([]int64, error) {
	return m.ids, nil
}

func (m *mockStore) ListSymbolMeta(ctx context.Context) ([]store.Symbol, error) {
	// The mock keeps full symbols; production strips text columns, which the
	// pipeline treats as an optimization only, so serving them is harmless.
	if m.metaCache != nil {
		return m.metaCache, nil
	}
	return m.GetSymbolsByIDs(ctx, m.ids)
}

func (m *mockStore) ListAllEdges(_ context.Context) ([]store.Edge, error) {
	return m.edges, nil
}

func (m *mockStore) SearchByText(_ context.Context, query string, _ int) ([]store.ScoredSymbol, error) {
	if m.onSearchByText != nil {
		m.onSearchByText(query)
	}
	// ftsByQuery lets a test give different terms different hits, which is what
	// the literal channel is about: agreement BETWEEN identifiers in one file.
	if m.ftsByQuery != nil {
		return m.ftsByQuery[query], nil
	}
	return m.ftsResult, nil
}

func (m *mockStore) GetEmbeddingsByIDs(_ context.Context, _ []int64) (map[int64][]float32, error) {
	return map[int64][]float32{}, nil
}

func (m *mockStore) GetCallerEdges(_ context.Context, dstIDs []int64, limitPerSymbol int) (map[int64][]int64, error) {
	result := make(map[int64][]int64, len(dstIDs))
	dstSet := make(map[int64]bool, len(dstIDs))
	for _, id := range dstIDs {
		dstSet[id] = true
	}
	counts := make(map[int64]int)
	for _, e := range m.edges {
		if dstSet[e.Dst] && (limitPerSymbol <= 0 || counts[e.Dst] < limitPerSymbol) {
			result[e.Dst] = append(result[e.Dst], e.Src)
			counts[e.Dst]++
		}
	}
	return result, nil
}

func (m *mockStore) GetCalleeEdges(_ context.Context, srcIDs []int64, limitPerSymbol int) (map[int64][]int64, error) {
	result := make(map[int64][]int64, len(srcIDs))
	srcSet := make(map[int64]bool, len(srcIDs))
	for _, id := range srcIDs {
		srcSet[id] = true
	}
	counts := make(map[int64]int)
	for _, e := range m.edges {
		if srcSet[e.Src] && (limitPerSymbol <= 0 || counts[e.Src] < limitPerSymbol) {
			result[e.Src] = append(result[e.Src], e.Dst)
			counts[e.Src]++
		}
	}
	return result, nil
}

type mockEmbedder struct{}

func (e *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, 4)
		out[i][0] = 1.0
	}
	return out, nil
}

type mockReranker struct {
	order     []string
	seenCount int
}

func (m *mockReranker) Rerank(_ context.Context, _ string, candidates []ScoredResult) ([]ScoredResult, error) {
	m.seenCount = len(candidates)
	byName := make(map[string]ScoredResult, len(candidates))
	for _, c := range candidates {
		byName[c.QualifiedName] = c
	}
	var out []ScoredResult
	for _, name := range m.order {
		if c, ok := byName[name]; ok {
			c.Score += 10
			c.Why += "+rerank"
			out = append(out, c)
			delete(byName, name)
		}
	}
	for _, c := range candidates {
		if _, ok := byName[c.QualifiedName]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestRetriever_BasicFlow(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 10, BodyExcerpt: "func Alpha() {}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 11, EndLine: 20, BodyExcerpt: "func Beta() {}"},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 21, EndLine: 30, BodyExcerpt: "func Gamma() {}"},
		{ID: 4, FileID: 11, Name: "Delta", Kind: "function", QualifiedName: "pkg.Delta", StartLine: 1, EndLine: 5, BodyExcerpt: "func Delta() {}"},
		{ID: 5, FileID: 11, Name: "Epsilon", Kind: "function", QualifiedName: "pkg.Epsilon", StartLine: 6, EndLine: 15, BodyExcerpt: "func Epsilon() {}"},
	}
	edges := []store.Edge{
		{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1},
		{Src: 2, Dst: 3, Kind: store.EdgeCalls, Weight: 1},
	}
	ids := []int64{1, 2, 3, 4, 5}
	filePaths := map[int64]string{10: "pkg/alpha.go", 11: "pkg/delta.go"}

	ms := &mockStore{symbols: symbols, edges: edges, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        3,
		MaxResults:   10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Symbols)

	// symbol 1 must appear as vector_seed (highest vector score, first seed)
	byName := make(map[string]ScoredResult)
	for _, s := range result.Symbols {
		byName[s.QualifiedName] = s
	}

	alpha, ok := byName["pkg.Alpha"]
	require.True(t, ok, "pkg.Alpha must be in results")
	assert.Equal(t, "vector_seed", alpha.Why)

	// symbols 2 and 3 should appear (connected by edges to seed 1→2→3)
	_, has2 := byName["pkg.Beta"]
	_, has3 := byName["pkg.Gamma"]
	assert.True(t, has2 || has3, "at least one of Beta/Gamma should appear via PPR propagation")

	// verify non-seed symbols get ppr_neighbor label
	for _, s := range result.Symbols {
		if s.QualifiedName != "pkg.Alpha" && s.QualifiedName != "pkg.Beta" && s.QualifiedName != "pkg.Gamma" {
			assert.Equal(t, "ppr_neighbor", s.Why)
		}
	}

	assert.Greater(t, result.TotalTokens, 0)
	assert.Greater(t, result.Stats.PPRIterations, 0)
	assert.Equal(t, 5, result.Stats.GraphNodes)
}

func TestRetriever_KeepsFullExpansionSnapshotBeforeEvidenceTrim(t *testing.T) {
	body := strings.Repeat("line\n", 39) + "line"
	ms := &mockStore{
		symbols:   []store.Symbol{{ID: 1, FileID: 10, Name: "Long", Kind: "function", QualifiedName: "pkg.Long", StartLine: 100, EndLine: 139, BodyExcerpt: body}},
		ids:       []int64{1},
		filePaths: map[int64]string{10: "pkg/long.go"},
	}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{Query: "long function", BudgetTokens: 10000, SeedK: 1, MaxResults: 1})
	require.NoError(t, err)
	require.Len(t, result.Symbols, 1)
	require.Len(t, result.ExpansionSymbols, 1)
	assert.Equal(t, "excerpt", result.Symbols[0].Detail)
	assert.NotEqual(t, body, result.Symbols[0].Body)
	assert.Equal(t, "full", result.ExpansionSymbols[0].Detail)
	assert.Equal(t, body, result.ExpansionSymbols[0].Body)

	full, err := r.Retrieve(context.Background(), Request{
		Query: "long function", BudgetTokens: 10000, SeedK: 1, MaxResults: 1,
		FullBodyResults: 1, PreserveFullBodies: true,
	})
	require.NoError(t, err)
	require.Len(t, full.Symbols, 1)
	assert.Equal(t, "full", full.Symbols[0].Detail)
	assert.Equal(t, body, full.Symbols[0].Body)
}

func TestRRF_KnownInputs(t *testing.T) {
	list1 := []int64{1, 2, 3}
	list2 := []int64{3, 1, 4}
	scores := rrf([][]int64{list1, list2})

	s1 := 1.0/float32(rrfK+1) + 1.0/float32(rrfK+2)
	s3 := 1.0/float32(rrfK+3) + 1.0/float32(rrfK+1)

	if scores[1] != s1 {
		t.Errorf("id=1 score=%v want %v", scores[1], s1)
	}
	if scores[3] != s3 {
		t.Errorf("id=3 score=%v want %v", scores[3], s3)
	}
	if scores[4] == 0 {
		t.Error("id=4 should have nonzero score")
	}
}

func TestTokenizeForOverlapSplitsCamelAndNormalizesPlural(t *testing.T) {
	got := tokenizeForOverlap("GetEmployees parseCSVMainInfo child indices initialized")

	require.Contains(t, got, "get")
	require.Contains(t, got, "employee")
	require.Contains(t, got, "parse")
	require.Contains(t, got, "csv")
	require.Contains(t, got, "main")
	require.Contains(t, got, "info")
	require.Contains(t, got, "child")
	require.Contains(t, got, "index")
	require.Contains(t, got, "new")
}

func TestOverlapRatioUsesNormalizedFieldTokens(t *testing.T) {
	got := overlapRatio(tokenizeForOverlap("returns employee list"), "GetEmployees")

	require.Greater(t, got, float32(0))
}

func TestEffectiveAlphaRaisesGraphGateForNearExactDirectMatch(t *testing.T) {
	alpha := effectiveAlpha(Request{Query: "BuyBuildHandler BuildAndBuy"}, []store.ScoredSymbol{
		{Symbol: store.Symbol{Name: "BuildAndBuy", QualifiedName: "api.BuyBuildHandler.BuildAndBuy", Signature: "func (h *BuyBuildHandler) BuildAndBuy()"}, Score: 1.0},
		{Symbol: store.Symbol{Name: "Generate", QualifiedName: "inventory.TradeUpGenerator.Generate"}, Score: 0.25},
	})

	require.GreaterOrEqual(t, alpha, float32(0.9))
}

func TestEffectiveAlphaKeepsGraphUsefulForWeakDirectMatch(t *testing.T) {
	alpha := effectiveAlpha(Request{Query: "where does buying flow connect"}, []store.ScoredSymbol{
		{Symbol: store.Symbol{Name: "Generate", QualifiedName: "inventory.TradeUpGenerator.Generate"}, Score: 1.0},
		{Symbol: store.Symbol{Name: "Run", QualifiedName: "worker.Run"}, Score: 0.95},
	})

	require.LessOrEqual(t, alpha, float32(0.7))
}

func TestEffectiveAlphaHonorsExplicitAlpha(t *testing.T) {
	alpha := effectiveAlpha(Request{Query: "handler builds", Alpha: 0.42, AlphaSet: true}, []store.ScoredSymbol{
		{Symbol: store.Symbol{Name: "BuildAndBuy", QualifiedName: "api.BuyBuildHandler.BuildAndBuy"}, Score: 1.0},
	})

	require.Equal(t, float32(0.42), alpha)
}

func TestRetriever_HybridMode(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha() {}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta() {}"},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 11, EndLine: 15, BodyExcerpt: "func Gamma() {}"},
	}
	ids := []int64{1, 2, 3}
	filePaths := map[int64]string{10: "pkg/file.go"}

	ms := &mockStore{
		symbols:   symbols,
		ids:       ids,
		filePaths: filePaths,
		ftsResult: []store.ScoredSymbol{
			{Symbol: symbols[2], Score: 5.0},
			{Symbol: symbols[0], Score: 3.0},
		},
	}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "gamma alpha function",
		BudgetTokens: 10000,
		SeedK:        5,
		MaxResults:   10,
		Mode:         ModeHybrid,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Symbols)

	byName := make(map[string]ScoredResult)
	for _, s := range result.Symbols {
		byName[s.QualifiedName] = s
	}

	alpha, ok := byName["pkg.Alpha"]
	require.True(t, ok, "Alpha should be in results")
	assert.Equal(t, "hybrid_seed", alpha.Why, "Alpha appears in both vector and FTS seeds")
}

func TestRetriever_HybridProtectsTopVectorSeedFromGraphNoise(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Direct", Kind: "function", QualifiedName: "pkg.Direct", StartLine: 1, EndLine: 20, BodyExcerpt: strings.Repeat("direct ", 20)},
		{ID: 2, FileID: 10, Name: "NoisyHub", Kind: "function", QualifiedName: "pkg.NoisyHub", StartLine: 21, EndLine: 40, BodyExcerpt: strings.Repeat("hub ", 20)},
		{ID: 3, FileID: 10, Name: "Helper", Kind: "function", QualifiedName: "pkg.Helper", StartLine: 41, EndLine: 60, BodyExcerpt: strings.Repeat("helper ", 20)},
	}
	ms := &mockStore{
		symbols:   symbols,
		ids:       []int64{1, 2, 3},
		filePaths: map[int64]string{10: "pkg/file.go"},
		edges: []store.Edge{
			{Src: 2, Dst: 3, Kind: store.EdgeCalls, Weight: 1},
		},
	}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "unrelated wording",
		BudgetTokens: 10000,
		SeedK:        3,
		MaxResults:   1,
		Mode:         ModeHybrid,
	})
	require.NoError(t, err)
	require.Len(t, result.Symbols, 1)
	require.Equal(t, "pkg.Direct", result.Symbols[0].QualifiedName)
}

func TestRetriever_AlphaOne_SeedOrderDominates(t *testing.T) {
	// alpha=1.0: output order must match seed order (highest vector score first).
	// PPR has zero weight, so graph topology is irrelevant.
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha(){}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta(){}"},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 11, EndLine: 15, BodyExcerpt: "func Gamma(){}"},
	}
	// edges: 3→2→1 so PPR would push Gamma highest if it mattered
	edges := []store.Edge{
		{Src: 3, Dst: 2, Kind: store.EdgeCalls, Weight: 1},
		{Src: 2, Dst: 1, Kind: store.EdgeCalls, Weight: 1},
	}
	ids := []int64{1, 2, 3}
	filePaths := map[int64]string{10: "pkg/file.go"}

	ms := &mockStore{symbols: symbols, edges: edges, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        3,
		MaxResults:   3,
		Alpha:        1.0,
		AlphaSet:     true,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(result.Symbols), 2)

	// mockEmbedder gives score 1/1, 1/2, 1/3 for ids 1,2,3 → Alpha scores highest
	assert.Equal(t, "pkg.Alpha", result.Symbols[0].QualifiedName, "alpha=1.0 must put top seed first")
}

func TestRetriever_AlphaZero_PPRDominates(t *testing.T) {
	// alpha=0.0: only PPR score counts, seeds get no direct bonus.
	// We verify that non-seed nodes can outrank seeds when graph topology favors them.
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha(){}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta(){}"},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 11, EndLine: 15, BodyExcerpt: "func Gamma(){}"},
	}
	ids := []int64{1, 2, 3}
	filePaths := map[int64]string{10: "pkg/file.go"}

	ms := &mockStore{symbols: symbols, edges: nil, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        3,
		MaxResults:   3,
		Alpha:        0.0,
		AlphaSet:     true,
	})
	require.NoError(t, err)
	// alpha=0: final_score = norm_ppr only, all 3 symbols must still appear
	assert.Len(t, result.Symbols, 3)
}

func TestRetriever_AlphaZeroDoesNotApplyDefaultSeedWeight(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha(){\nprintln(\"alpha\")\n}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta(){\nprintln(\"beta\")\n}"},
	}
	edges := []store.Edge{{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1}}
	ids := []int64{1, 2}
	filePaths := map[int64]string{10: "pkg/file.go"}

	ms := &mockStore{symbols: symbols, edges: edges, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        1,
		MaxResults:   2,
		Alpha:        0.0,
		AlphaSet:     true,
	})
	require.NoError(t, err)

	var beta ScoredResult
	for _, sym := range result.Symbols {
		if sym.QualifiedName == "pkg.Beta" {
			beta = sym
			break
		}
	}
	require.Equal(t, "pkg.Beta", beta.QualifiedName)
	require.Greater(t, beta.Score, float32(0.8), "alpha=0 must use normalized PPR, not default seed weighting")
}

func TestRetriever_AlphaHalf_BothContribute(t *testing.T) {
	// alpha=0.5: seed score and PPR both contribute equally.
	// Smoke test: results are returned and Why labels are correct.
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha(){}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta(){}"},
	}
	ids := []int64{1, 2}
	filePaths := map[int64]string{10: "pkg/file.go"}

	ms := &mockStore{symbols: symbols, edges: nil, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        2,
		MaxResults:   2,
		Alpha:        0.5,
		AlphaSet:     true,
	})
	require.NoError(t, err)
	require.Len(t, result.Symbols, 2)

	byName := make(map[string]ScoredResult)
	for _, s := range result.Symbols {
		byName[s.QualifiedName] = s
	}
	_, hasAlpha := byName["pkg.Alpha"]
	_, hasBeta := byName["pkg.Beta"]
	assert.True(t, hasAlpha && hasBeta, "both symbols must appear with alpha=0.5")
}

func TestRetriever_RerankerReordersCandidatesBeforePacking(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha(){\nprintln(\"alpha\")\n}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta(){\nprintln(\"beta\")\n}"},
	}
	ids := []int64{1, 2}
	filePaths := map[int64]string{10: "pkg/file.go"}

	ms := &mockStore{symbols: symbols, ids: ids, filePaths: filePaths}
	r := NewRetrieverWithReranker(ms, &mockEmbedder{}, &mockReranker{order: []string{"pkg.Beta"}}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        2,
		MaxResults:   2,
		Alpha:        1.0,
		AlphaSet:     true,
	})
	require.NoError(t, err)
	require.Len(t, result.Symbols, 2)
	assert.Equal(t, "pkg.Beta", result.Symbols[0].QualifiedName)
	assert.Contains(t, result.Symbols[0].Why, "rerank")
}

func TestRetriever_AdaptiveRerankSkipsConfidentConstructorMatch(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "NewAlpha", Kind: "function", QualifiedName: "pkg.NewAlpha", StartLine: 1, EndLine: 10, BodyExcerpt: "func NewAlpha() *Alpha { return &Alpha{} }"},
		{ID: 2, FileID: 10, Name: "CreateAlpha", Kind: "function", QualifiedName: "pkg.CreateAlpha", StartLine: 11, EndLine: 20, BodyExcerpt: "func CreateAlpha() *Alpha { return NewAlpha() }"},
		{ID: 3, FileID: 10, Name: "Alpha", Kind: "class", QualifiedName: "pkg.Alpha", StartLine: 21, EndLine: 30, BodyExcerpt: "type Alpha struct{}"},
	}
	ids := []int64{1, 2, 3}
	filePaths := map[int64]string{10: "pkg/alpha.go"}
	ms := &mockStore{symbols: symbols, ids: ids, filePaths: filePaths}
	rr := &mockReranker{order: []string{"pkg.CreateAlpha", "pkg.NewAlpha"}}
	r := NewRetrieverWithReranker(ms, &mockEmbedder{}, rr, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:          "NewAlpha constructor",
		BudgetTokens:   10000,
		SeedK:          3,
		MaxResults:     3,
		RerankK:        3,
		AdaptiveRerank: true,
	})

	require.NoError(t, err)
	require.NotEmpty(t, result.Symbols)
	require.Equal(t, 0, rr.seenCount)
	require.Equal(t, "pkg.NewAlpha", result.Symbols[0].QualifiedName)
}

func TestRetriever_RerankKExpandsCandidatePoolWithoutExpandingResults(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 5, BodyExcerpt: "func Alpha(){\nprintln(\"alpha alpha alpha alpha alpha alpha alpha alpha\")\n}"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 6, EndLine: 10, BodyExcerpt: "func Beta(){\nprintln(\"beta beta beta beta beta beta beta beta\")\n}"},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 11, EndLine: 15, BodyExcerpt: "func Gamma(){\nprintln(\"gamma gamma gamma gamma gamma gamma gamma gamma\")\n}"},
		{ID: 4, FileID: 10, Name: "Delta", Kind: "function", QualifiedName: "pkg.Delta", StartLine: 16, EndLine: 20, BodyExcerpt: "func Delta(){\nprintln(\"delta delta delta delta delta delta delta delta\")\n}"},
		{ID: 5, FileID: 10, Name: "Epsilon", Kind: "function", QualifiedName: "pkg.Epsilon", StartLine: 21, EndLine: 25, BodyExcerpt: "func Epsilon(){\nprintln(\"epsilon epsilon epsilon epsilon epsilon epsilon\")\n}"},
	}
	ids := []int64{1, 2, 3, 4, 5}
	filePaths := map[int64]string{10: "pkg/file.go"}

	rr := &mockReranker{order: []string{"pkg.Delta"}}
	ms := &mockStore{symbols: symbols, ids: ids, filePaths: filePaths}
	r := NewRetrieverWithReranker(ms, &mockEmbedder{}, rr, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 10000,
		SeedK:        5,
		MaxResults:   2,
		RerankK:      4,
		Alpha:        1.0,
		AlphaSet:     true,
	})
	require.NoError(t, err)
	require.Len(t, result.Symbols, 2)
	// RerankK(4) + the remaining top-PPR candidate appended by appendMissingTopPPR.
	require.Equal(t, 5, rr.seenCount)
	require.Equal(t, "pkg.Delta", result.Symbols[0].QualifiedName)
}

func TestRetriever_PopulatesRankingFeatures(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "SearchByVector", Kind: "function", QualifiedName: "sqlite.SearchByVector", StartLine: 1, EndLine: 8, Signature: "func SearchByVector(ctx context.Context)", BodyExcerpt: "func SearchByVector(ctx context.Context) {}"},
	}
	ids := []int64{1}
	filePaths := map[int64]string{10: "internal/store/sqlite/sqlite.go"}
	ms := &mockStore{symbols: symbols, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "sqlite vector search",
		BudgetTokens: 10000,
		SeedK:        1,
		MaxResults:   1,
		Alpha:        1.0,
		AlphaSet:     true,
	})

	require.NoError(t, err)
	require.Len(t, result.Symbols, 1)
	require.Greater(t, result.Symbols[0].Features.NameOverlap, float32(0))
	require.Greater(t, result.Symbols[0].Features.PathOverlap, float32(0))
	require.Equal(t, float32(1), result.Symbols[0].Features.VectorSeed)
}

func TestRetriever_TokenBudget(t *testing.T) {
	symbols := make([]store.Symbol, 20)
	ids := make([]int64, 20)
	for i := range symbols {
		id := int64(i + 1)
		ids[i] = id
		symbols[i] = store.Symbol{
			ID:            id,
			FileID:        1,
			QualifiedName: "pkg.Sym" + string(rune('A'+i)),
			Kind:          "function",
			StartLine:     i*10 + 1,
			EndLine:       i*10 + 10,
			BodyExcerpt:   "func Sym() { /* some body that takes tokens */ }",
		}
	}
	ms := &mockStore{
		symbols:   symbols,
		ids:       ids,
		edges:     nil,
		filePaths: map[int64]string{1: "pkg/file.go"},
	}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "anything",
		BudgetTokens: 50,
		SeedK:        5,
		MaxResults:   20,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, result.TotalTokens, 50)
}

func TestRetriever_OutputModeMinimal_NoGraphContext(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 10, BodyExcerpt: "func Alpha() { Beta() }"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 11, EndLine: 20, BodyExcerpt: "func Beta() {}"},
	}
	edges := []store.Edge{{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1}}
	ids := []int64{1, 2}
	filePaths := map[int64]string{10: "pkg/alpha.go"}
	ms := &mockStore{symbols: symbols, edges: edges, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "alpha beta",
		BudgetTokens: 10000,
		SeedK:        2,
		MaxResults:   2,
		OutputMode:   OutputModeMinimal,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Symbols)

	for _, s := range result.Symbols {
		assert.Empty(t, s.Callers, "minimal mode must not populate Callers")
		assert.Empty(t, s.Callees, "minimal mode must not populate Callees")
	}
	assert.Nil(t, result.NextSteps, "minimal mode must not populate NextSteps")
	assert.Nil(t, result.Structure, "minimal mode must not populate Structure")
}

func TestRetriever_OutputModeAnswer_HasGraphContext(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 10, BodyExcerpt: "func Alpha() { Beta() }"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 11, EndLine: 20, BodyExcerpt: "func Beta() {}"},
	}
	edges := []store.Edge{{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1}}
	ids := []int64{1, 2}
	filePaths := map[int64]string{10: "pkg/alpha.go"}
	ms := &mockStore{symbols: symbols, edges: edges, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "alpha beta",
		BudgetTokens: 10000,
		SeedK:        2,
		MaxResults:   2,
		OutputMode:   OutputModeAnswer,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Symbols)

	hasConfidence := false
	for _, s := range result.Symbols {
		if s.Confidence != "" {
			hasConfidence = true
		}
	}
	assert.True(t, hasConfidence, "answer mode must assign Confidence")
	assert.NotNil(t, result.NextSteps, "answer mode must populate NextSteps")
	assert.Nil(t, result.Structure, "answer mode must not populate Structure")
}

func TestEnrichGraphContextMarksStaticEdgesAndIncludesTopCallSites(t *testing.T) {
	alphaBody := strings.Join([]string{
		"func Alpha(format int) {",
		"Prepare()",
		"Validate()",
		"switch format {",
		"case batchResponse:",
		"Beta()",
		"case columnarResponse:",
		"Gamma()",
		"}",
		"}",
	}, "\n")
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 10, EndLine: 19, BodyExcerpt: "func Alpha", FullBody: alphaBody},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 20, EndLine: 20, BodyExcerpt: "func Beta() {}"},
		{ID: 3, FileID: 10, Name: "Gamma", Kind: "function", QualifiedName: "pkg.Gamma", StartLine: 30, EndLine: 30, BodyExcerpt: "func Gamma() {}"},
		{ID: 4, FileID: 10, Name: "Prepare", Kind: "function", QualifiedName: "pkg.Prepare", StartLine: 40, EndLine: 40, BodyExcerpt: "func Prepare() {}"},
		{ID: 5, FileID: 10, Name: "Validate", Kind: "function", QualifiedName: "pkg.Validate", StartLine: 50, EndLine: 50, BodyExcerpt: "func Validate() {}"},
	}
	ms := &mockStore{
		symbols: symbols,
		edges: []store.Edge{
			{Src: 1, Dst: 4, Kind: store.EdgeCalls, Weight: 1, CallLine: 11},
			{Src: 1, Dst: 5, Kind: store.EdgeCalls, Weight: 1, CallLine: 12},
			{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1, CallLine: 15},
			{Src: 1, Dst: 3, Kind: store.EdgeCalls, Weight: 1, CallLine: 17},
		},
		ids:       []int64{1, 2, 3, 4, 5},
		filePaths: map[int64]string{10: "pkg/alpha.go"},
	}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())
	scored := []ScoredResult{
		{SymbolID: 1, QualifiedName: "pkg.Alpha"},
		{SymbolID: 2, QualifiedName: "pkg.Beta"},
	}

	require.NoError(t, enrichGraphContext(context.Background(), r, Request{}, scored, ms.filePaths))

	// The top result is where a call chain gets followed, so it carries the
	// branch evidence.
	require.Len(t, scored[0].Callees, 4, "callee limit must retain both branch alternatives after helper calls")
	assert.Empty(t, scored[0].Callees[0].CallSite, "unconditional helpers should not add evidence payload")
	assert.Empty(t, scored[0].Callees[1].CallSite, "unconditional helpers should not add evidence payload")
	assert.Equal(t, "static_unverified", scored[0].Callees[2].PathStatus)
	assert.Contains(t, scored[0].Callees[2].CallSite, "14 case batchResponse:")
	assert.Contains(t, scored[0].Callees[2].CallSite, "15 Beta()")
	assert.Equal(t, "static_unverified", scored[0].Callees[3].PathStatus)
	assert.Contains(t, scored[0].Callees[3].CallSite, "16 case columnarResponse:")
	assert.Contains(t, scored[0].Callees[3].CallSite, "17 Gamma()")

	// Below it the refs stay — they are cheap and they are the navigation — but
	// the source windows do not. Hydrating them for every result measured at 205
	// of 1028 response tokens on cockroach (cmd/rspbreak), spent showing branches
	// around calls made by candidates the agent did not pick.
	require.Len(t, scored[1].Callers, 1)
	assert.Equal(t, "static_unverified", scored[1].Callers[0].PathStatus)
	assert.Equal(t, 15, scored[1].Callers[0].CallLine, "the call line itself is kept")
	assert.Empty(t, scored[1].Callers[0].CallSite, "only the top result pays for callsite windows")
}

func TestRetriever_OutputModeExplore_HasStructure(t *testing.T) {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Alpha", Kind: "function", QualifiedName: "pkg.Alpha", StartLine: 1, EndLine: 10, BodyExcerpt: "func Alpha() { Beta() }"},
		{ID: 2, FileID: 10, Name: "Beta", Kind: "function", QualifiedName: "pkg.Beta", StartLine: 11, EndLine: 20, BodyExcerpt: "func Beta() {}"},
	}
	ids := []int64{1, 2}
	filePaths := map[int64]string{10: "pkg/alpha.go"}
	ms := &mockStore{symbols: symbols, ids: ids, filePaths: filePaths}
	r := NewRetriever(ms, &mockEmbedder{}, slog.Default())

	result, err := r.Retrieve(context.Background(), Request{
		Query:        "alpha beta",
		BudgetTokens: 10000,
		SeedK:        2,
		MaxResults:   2,
		OutputMode:   OutputModeExplore,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Symbols)

	assert.NotNil(t, result.Structure, "explore mode must populate Structure")
	assert.NotNil(t, result.NextSteps, "explore mode must populate NextSteps")
}

func TestParseOutputMode(t *testing.T) {
	cases := []struct {
		input string
		want  OutputMode
		isErr bool
	}{
		{"answer", OutputModeAnswer, false},
		{"", OutputModeAnswer, false},
		{"minimal", OutputModeMinimal, false},
		{"explore", OutputModeExplore, false},
		{"invalid", OutputModeAnswer, true},
	}
	for _, tc := range cases {
		got, err := ParseOutputMode(tc.input)
		if tc.isErr {
			assert.Error(t, err, "input=%q", tc.input)
		} else {
			assert.NoError(t, err, "input=%q", tc.input)
			assert.Equal(t, tc.want, got, "input=%q", tc.input)
		}
	}
}

func TestPack_PreservesRankingOrder(t *testing.T) {
	symbols := []ScoredResult{
		{QualifiedName: "pkg.HighScoreLongBody", Score: 10, Body: strings.Repeat("x", 400)},
		{QualifiedName: "pkg.LowerScoreShortBody", Score: 9, Body: "short"},
	}

	selected, _ := Pack(symbols, 1000, 0)

	require.Len(t, selected, 2)
	require.Equal(t, "pkg.HighScoreLongBody", selected[0].QualifiedName)
	require.Equal(t, "pkg.LowerScoreShortBody", selected[1].QualifiedName)
}

func TestPack_TiersBodiesBeyondFullBodyCount(t *testing.T) {
	long := strings.Repeat("body line\n", 50)
	symbols := []ScoredResult{
		{QualifiedName: "pkg.A", Body: long, Signature: "func A() error"},
		{QualifiedName: "pkg.B", Body: long, Signature: "func B() error"},
		{QualifiedName: "pkg.C", Body: long, Signature: "func C() error", Docstring: "C does things.\nLong tail."},
		{QualifiedName: "pkg.D", Body: long, Signature: "func D() error", Docstring: "D drives.\nMore."},
	}

	selected, total := Pack(symbols, 100000, 2)
	require.Len(t, selected, 4)
	require.Equal(t, "full", selected[0].Detail)
	require.Equal(t, "full", selected[1].Detail)
	require.Equal(t, long, selected[1].Body)
	require.Equal(t, "compact", selected[2].Detail)
	require.Equal(t, "func C() error\n// C does things.", selected[2].Body)
	require.Equal(t, "compact", selected[3].Detail)
	require.Less(t, total, 2*estimateTokens(long)+100, "tail must not pay full body cost")

	// negative count disables tiering
	allFull, _ := Pack(symbols, 100000, -1)
	for i := range allFull {
		require.Equal(t, "full", allFull[i].Detail)
		require.Equal(t, long, allFull[i].Body)
	}
}

func TestApplyEvidenceSpansMarksVisibleExcerpt(t *testing.T) {
	// One window: this covers the line-numbering bookkeeping, not how many
	// windows a spread-out answer earns.
	t.Setenv("CONTEXTMAXXER_EVIDENCE_WINDOWS", "1")
	body := strings.Repeat("line\n", 39) + "line"
	results := []ScoredResult{{
		QualifiedName: "pkg.Long",
		StartLine:     100,
		EndLine:       139,
		Body:          body,
		Detail:        "full",
	}}
	r := &Retriever{embedder: &mockEmbedder{}}

	applyEvidenceSpans(context.Background(), r, "", []float32{1, 0, 0, 0}, results)

	require.Equal(t, "excerpt", results[0].Detail)
	require.Equal(t, 100, results[0].BodyStartLine)
	require.Equal(t, 112, results[0].BodyEndLine)
	require.Len(t, strings.Split(results[0].Body, "\n"), 13)
}

// needleEmbedder scores only the window that contains the needle, standing in
// for a real query embedding that matches one region of a function.
type needleEmbedder struct{ needle string }

func (e *needleEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = make([]float32, 4)
		if strings.Contains(t, e.needle) {
			out[i][0] = 1.0
		} else {
			out[i][1] = 1.0
		}
	}
	return out, nil
}

type failingEmbedder struct{}

func (e *failingEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("embed unavailable")
}

// Regression: the indexed body is capped, so windowing it could only ever
// surface the head of a long symbol. Measured before the fix, the shown span
// contained the queried code in 3% of deep-content cases on prometheus and 0%
// on cockroach (cmd/deepprobe).
func TestApplyEvidenceSpansWindowsTheLosslessBody(t *testing.T) {
	head := strings.Repeat("head\n", 30)
	full := head + strings.Repeat("tail\n", 25) + "NEEDLE here\n" + strings.Repeat("tail\n", 20)
	r := &Retriever{
		embedder: &needleEmbedder{needle: "NEEDLE"},
		store: &mockStore{symbols: []store.Symbol{{
			ID: 7, BodyExcerpt: head, FullBody: full,
		}}},
	}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 176,
		Body: head, Detail: "full",
	}}

	applyEvidenceSpans(context.Background(), r, "", []float32{1, 0, 0, 0}, results)

	require.Contains(t, results[0].Body, "NEEDLE", "the window must come from the lossless body")
	require.Greater(t, results[0].BodyStartLine, 130, "the head is not where the answer was")
	require.LessOrEqual(t, results[0].BodyStartLine, 155)
	require.GreaterOrEqual(t, results[0].BodyEndLine, 155)
}

// Hydrating before scoring means a scoring failure would otherwise ship the
// whole function — the exact payload the cap exists to prevent.
func TestApplyEvidenceSpansRestoresExcerptWhenScoringFails(t *testing.T) {
	head := strings.Repeat("head\n", 30)
	full := head + strings.Repeat("tail\n", 200)
	r := &Retriever{
		embedder: &failingEmbedder{},
		store: &mockStore{symbols: []store.Symbol{{
			ID: 7, BodyExcerpt: head, FullBody: full,
		}}},
	}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 330,
		Body: head, Detail: "full",
	}}

	applyEvidenceSpans(context.Background(), r, "", []float32{1, 0, 0, 0}, results)

	require.Equal(t, head, results[0].Body, "a failed scoring pass must not leave the body hydrated")
}

// needleScorer is a Reranker that also scores free-form texts, standing in for
// the loaded cross-encoder.
type needleScorer struct {
	needle string
	fail   bool
}

func (s *needleScorer) Rerank(_ context.Context, _ string, c []ScoredResult) ([]ScoredResult, error) {
	return c, nil
}

func (s *needleScorer) ScoreTexts(_ context.Context, _ string, docs []string) ([]float32, error) {
	if s.fail {
		return nil, errors.New("scorer unavailable")
	}
	out := make([]float32, len(docs))
	for i, d := range docs {
		if strings.Contains(d, s.needle) {
			out[i] = 1.0
		}
	}
	return out, nil
}

func decoyBody() string {
	return strings.Repeat("head\n", 30) + "DECOY here\n" + strings.Repeat("mid\n", 18) +
		"NEEDLE here\n" + strings.Repeat("tail\n", 10)
}

// The cross-encoder reads query and window together; the bi-encoder compares
// vectors built in ignorance of each other. When they disagree the
// cross-encoder must decide, or the second stage is decorative.
func TestApplyEvidenceSpansPrefersCrossEncoderWindow(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_EVIDENCE_CE_TOPK", "0") // score every window
	full := decoyBody()
	r := &Retriever{
		embedder: &needleEmbedder{needle: "DECOY"},
		reranker: &needleScorer{needle: "NEEDLE"},
		store: &mockStore{symbols: []store.Symbol{{
			ID: 7, BodyExcerpt: strings.Repeat("head\n", 30), FullBody: full,
		}}},
	}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 160,
		Body: strings.Repeat("head\n", 30), Detail: "full",
	}}

	applyEvidenceSpans(context.Background(), r, "where is the needle", []float32{1, 0, 0, 0}, results)

	require.Contains(t, results[0].Body, "NEEDLE", "the cross-encoder pick must win")
	require.NotContains(t, results[0].Body, "DECOY")
}

// A cross-encoder failure must cost the refinement, not the trim.
func TestApplyEvidenceSpansFallsBackToBiEncoderWindow(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_EVIDENCE_CE_TOPK", "0")
	full := decoyBody()
	r := &Retriever{
		embedder: &needleEmbedder{needle: "DECOY"},
		reranker: &needleScorer{needle: "NEEDLE", fail: true},
		store: &mockStore{symbols: []store.Symbol{{
			ID: 7, BodyExcerpt: strings.Repeat("head\n", 30), FullBody: full,
		}}},
	}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 160,
		Body: strings.Repeat("head\n", 30), Detail: "full",
	}}

	applyEvidenceSpans(context.Background(), r, "where is the needle", []float32{1, 0, 0, 0}, results)

	require.Contains(t, results[0].Body, "DECOY", "the bi-encoder choice must survive")
	require.Equal(t, "excerpt", results[0].Detail)
}

func (m *mockStore) SearchByBodyText(_ context.Context, _ string, _ int) ([]store.ScoredSymbol, error) {
	return m.bodyFTSResult, nil
}

func (m *mockStore) SearchByChunkVector(_ context.Context, _ []float32, _ int) ([]store.ScoredSymbol, error) {
	return m.chunkVecResult, nil
}

func TestMergeSpansFoldsOverlappingWindows(t *testing.T) {
	// Adjacent winners must read as one block, not block / gap marker / the very
	// next line. Disjoint ones must stay apart.
	require.Equal(t,
		[]span{{0, 20}, {50, 66}},
		mergeSpans([]span{{50, 66}, {0, 16}, {10, 20}}))
	require.Nil(t, mergeSpans(nil))
}

// Regression: with three windows the answer can be in the runner-up. One window
// held the queried code 45% of the time on prometheus (cmd/deepprobe).
func TestApplyEvidenceSpansKeepsSeveralWindows(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_EVIDENCE_CE_TOPK", "0")
	head := strings.Repeat("head\n", 30)
	// The answer sits in two places far apart — the case several windows exist
	// for. One decisive region collapses to a single block instead, which is
	// what the keep-ratio is for.
	full := head + strings.Repeat("mid\n", 20) + "NEEDLE first\n" +
		strings.Repeat("gap\n", 40) + "NEEDLE second\n" + strings.Repeat("tail\n", 10)
	r := &Retriever{
		embedder: &needleEmbedder{needle: "head"},
		reranker: &needleScorer{needle: "NEEDLE"},
		store: &mockStore{symbols: []store.Symbol{{
			ID: 7, BodyExcerpt: head, FullBody: full,
		}}},
	}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 202,
		Body: head, Detail: "full",
	}}

	applyEvidenceSpans(context.Background(), r, "where is the needle", []float32{1, 0, 0, 0}, results)

	require.Contains(t, results[0].Body, "NEEDLE")
	require.Contains(t, results[0].Body, EvidenceGapMarker,
		"disjoint windows must be separated by the gap marker")
	require.GreaterOrEqual(t, len(results[0].BodySegments), 2)

	// Segments must describe exactly the lines Body carries, or the numbering
	// built from them lies.
	bodyLines := strings.Split(results[0].Body, "\n")
	total := 0
	for _, seg := range results[0].BodySegments {
		total += seg.Lines
	}
	require.Equal(t, len(bodyLines)-(len(results[0].BodySegments)-1), total,
		"body lines minus gap markers must equal the segment line counts")
}

func TestApplyEvidenceSpansSingleWindowLeavesNoSegments(t *testing.T) {
	t.Setenv("CONTEXTMAXXER_EVIDENCE_WINDOWS", "1")
	full := strings.Repeat("head\n", 30) + "NEEDLE here\n" + strings.Repeat("tail\n", 30)
	r := &Retriever{
		embedder: &needleEmbedder{needle: "NEEDLE"},
		store: &mockStore{symbols: []store.Symbol{{
			ID: 7, BodyExcerpt: strings.Repeat("head\n", 30), FullBody: full,
		}}},
	}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 161,
		Body: strings.Repeat("head\n", 30), Detail: "full",
	}}

	applyEvidenceSpans(context.Background(), r, "", []float32{1, 0, 0, 0}, results)

	require.Empty(t, results[0].BodySegments, "one window needs no segment list")
	require.NotContains(t, results[0].Body, EvidenceGapMarker)
}

// The mode that promises whole symbols shipped the capped excerpt while the
// header claimed the symbol's full line range: hydration lived inside window
// selection, which this path skips. Measured on cockroach before the fix, 42
// lines of a 128-line function presented as complete.
func TestHydrateFullBodiesFetchesTheLosslessBody(t *testing.T) {
	head := strings.Repeat("head\n", 30) + truncationMarker
	full := strings.Repeat("head\n", 30) + strings.Repeat("tail\n", 70)
	r := &Retriever{store: &mockStore{symbols: []store.Symbol{{
		ID: 7, BodyExcerpt: head, FullBody: full,
	}}}}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 200,
		Body: head, Detail: "full",
	}}

	hydrateFullBodies(context.Background(), r, results)

	require.NotContains(t, results[0].Body, truncationMarker, "full means full")
	require.Contains(t, results[0].Body, "tail")
	require.Equal(t, 100, results[0].BodyStartLine)
	require.Equal(t, 200, results[0].BodyEndLine)
}

// A legacy index has no lossless body to fetch. The excerpt is then all there
// is, and the response must report the lines it really carries rather than the
// symbol's extent.
func TestHydrateFullBodiesReportsWhatALegacyIndexCanGive(t *testing.T) {
	head := strings.Repeat("head\n", 29) + truncationMarker
	r := &Retriever{store: &mockStore{symbols: []store.Symbol{{
		ID: 7, BodyExcerpt: head,
	}}}}
	results := []ScoredResult{{
		SymbolID: 7, QualifiedName: "pkg.Long", StartLine: 100, EndLine: 200,
		Body: head, Detail: "full",
	}}

	hydrateFullBodies(context.Background(), r, results)

	require.Equal(t, "excerpt", results[0].Detail, "a capped body is an excerpt, whatever the caller asked for")
	require.Less(t, results[0].BodyEndLine, 200, "must not claim the symbol's whole range")
}

// graphContaminationFixture builds one hub symbol with three callees whose
// names sort differently under different queries.
func graphContaminationFixture() *mockStore {
	symbols := []store.Symbol{
		{ID: 1, FileID: 10, Name: "Hub", Kind: "function", QualifiedName: "pkg.Hub", StartLine: 1, EndLine: 10, BodyExcerpt: "func Hub() {}"},
		{ID: 2, FileID: 10, Name: "AlphaWriter", Kind: "function", QualifiedName: "pkg.AlphaWriter", StartLine: 11, EndLine: 20, BodyExcerpt: "func AlphaWriter() {}"},
		{ID: 3, FileID: 10, Name: "BetaReader", Kind: "function", QualifiedName: "pkg.BetaReader", StartLine: 21, EndLine: 30, BodyExcerpt: "func BetaReader() {}"},
		{ID: 4, FileID: 10, Name: "GammaSweeper", Kind: "function", QualifiedName: "pkg.GammaSweeper", StartLine: 31, EndLine: 40, BodyExcerpt: "func GammaSweeper() {}"},
	}
	edges := []store.Edge{
		{Src: 1, Dst: 2, Kind: store.EdgeCalls, Weight: 1, CallLine: 3},
		{Src: 1, Dst: 3, Kind: store.EdgeCalls, Weight: 1, CallLine: 4},
		{Src: 1, Dst: 4, Kind: store.EdgeCalls, Weight: 1, CallLine: 5},
	}
	return &mockStore{
		symbols:   symbols,
		edges:     edges,
		ids:       []int64{1, 2, 3, 4},
		filePaths: map[int64]string{10: "pkg/hub.go"},
		metaCache: symbols,
	}
}

func calleeNamesFor(t *testing.T, r *Retriever, query, want string) []string {
	t.Helper()
	res, err := r.Retrieve(context.Background(), Request{
		Query: query, BudgetTokens: 10000, SeedK: 4, MaxResults: 10,
	})
	require.NoError(t, err)
	for _, s := range res.Symbols {
		if s.QualifiedName != want {
			continue
		}
		names := make([]string, 0, len(s.Callees))
		for _, c := range s.Callees {
			names = append(names, c.QualifiedName)
		}
		return names
	}
	t.Fatalf("%q not in results for query %q", want, query)
	return nil
}

func TestRetriever_GraphRefsDoNotDependOnQueryHistory(t *testing.T) {
	// enrichGraphContext ranks a symbol's callees against the query. The edge
	// slices it ranks live in the process-wide graph memo, so sorting them in
	// place leaves one query's ordering behind for the next one. A query whose
	// tokens match no callee must see the same list whether or not some earlier
	// query reordered the cache.
	fresh := NewRetriever(graphContaminationFixture(), &mockEmbedder{}, slog.Default())
	baseline := calleeNamesFor(t, fresh, "zz", "pkg.Hub")

	used := NewRetriever(graphContaminationFixture(), &mockEmbedder{}, slog.Default())
	calleeNamesFor(t, used, "gamma sweeper", "pkg.Hub")
	after := calleeNamesFor(t, used, "zz", "pkg.Hub")

	require.Equal(t, baseline, after,
		"callee order leaked from the previous query through the shared graph memo")
}

func TestRetriever_ConcurrentRetrieveIsRaceFree(t *testing.T) {
	// The shipped MCP server dispatches tool calls on a worker pool, so two
	// Retrieve calls run against one Retriever at the same time. Run with -race.
	r := NewRetriever(graphContaminationFixture(), &mockEmbedder{}, slog.Default())
	queries := []string{"alpha writer", "beta reader", "gamma sweeper", "hub"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(q string) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, err := r.Retrieve(context.Background(), Request{
					Query: q, BudgetTokens: 10000, SeedK: 4, MaxResults: 10,
				})
				if err != nil {
					t.Errorf("retrieve %q: %v", q, err)
					return
				}
			}
		}(queries[i%len(queries)])
	}
	wg.Wait()
}
