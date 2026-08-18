package index_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/require"
)

type mockStore struct {
	store.Store
	files             []store.File
	symbolsByFile     map[int64][]store.Symbol
	symbolsByLanguage map[string][]store.Symbol
	embeddings        map[int64][]float32
	saveCalls         int
	symCalls          int
	embeddedIDs       []int64
	edges             []store.Edge
	nextSymID         int64
	contentVersion    int
}

func (m *mockStore) ListFiles(_ context.Context) ([]store.File, error) { return m.files, nil }
func (m *mockStore) SaveFile(_ context.Context, f *store.File) (int64, error) {
	m.saveCalls++
	for _, existing := range m.files {
		if existing.Path == f.Path {
			f.ID = existing.ID
			return existing.ID, nil
		}
	}
	return int64(m.saveCalls), nil
}
func (m *mockStore) SaveSymbolBatch(_ context.Context, symbols []store.Symbol) ([]int64, error) {
	m.symCalls++
	ids := make([]int64, len(symbols))
	for i := range ids {
		m.nextSymID++
		ids[i] = m.nextSymID
	}
	return ids, nil
}
func (m *mockStore) DeleteFile(_ context.Context, _ int64) error          { return nil }
func (m *mockStore) DeleteSymbolsByFile(_ context.Context, _ int64) error { return nil }
func (m *mockStore) ListSymbolsByFile(_ context.Context, fileID int64) ([]store.Symbol, error) {
	return m.symbolsByFile[fileID], nil
}
func (m *mockStore) ListSymbolsByLanguage(_ context.Context, language string) ([]store.Symbol, error) {
	return m.symbolsByLanguage[language], nil
}
func (m *mockStore) SaveEdgeBatch(_ context.Context, edges []store.Edge) error {
	m.edges = append(m.edges, edges...)
	return nil
}
func (m *mockStore) UpsertEmbedding(_ context.Context, symbolID int64, _ []float32) error {
	m.embeddedIDs = append(m.embeddedIDs, symbolID)
	return nil
}
func (m *mockStore) UpsertEmbeddingBatch(_ context.Context, embeddings []store.Embedding) error {
	for _, emb := range embeddings {
		m.embeddedIDs = append(m.embeddedIDs, emb.SymbolID)
	}
	return nil
}
func (m *mockStore) GetEmbedding(_ context.Context, symbolID int64) ([]float32, bool, error) {
	vec, ok := m.embeddings[symbolID]
	return vec, ok, nil
}
func (m *mockStore) SetIndexContentVersion(_ context.Context, version int) error {
	m.contentVersion = version
	return nil
}

type mockParser struct{}

func (m *mockParser) Parse(_ context.Context, _ []byte, _ string) (*tree_sitter.Tree, error) {
	return nil, nil
}
func (m *mockParser) LanguageNames() []string { return nil }
func (m *mockParser) Close() error            { return nil }

type mockEmbedder struct {
	texts []string
}

func (m *mockEmbedder) Dimension() int { return 384 }
func (m *mockEmbedder) Close() error   { return nil }
func (m *mockEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.texts = append(m.texts, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, 384)
	}
	return out, nil
}

type staticExtractor struct {
	symbols []store.Symbol
}

func (e staticExtractor) Symbols(_ *tree_sitter.Tree, _ []byte) []store.Symbol {
	return e.symbols
}

func (e staticExtractor) Edges(_ *tree_sitter.Tree, _ []byte, _ map[string]int64) []store.Edge {
	return nil
}

type sourceAwareExtractor struct {
	symbolsBySource map[string][]store.Symbol
	edgeForNames    map[string][2]string
	seenNameMaps    []map[string]int64
}

func (e *sourceAwareExtractor) Symbols(_ *tree_sitter.Tree, source []byte) []store.Symbol {
	return e.symbolsBySource[string(source)]
}

func (e *sourceAwareExtractor) Edges(_ *tree_sitter.Tree, source []byte, nameToID map[string]int64) []store.Edge {
	seen := make(map[string]int64, len(nameToID))
	for name, id := range nameToID {
		seen[name] = id
	}
	e.seenNameMaps = append(e.seenNameMaps, seen)
	names, ok := e.edgeForNames[string(source)]
	if !ok {
		return nil
	}
	src, srcOK := nameToID[names[0]]
	dst, dstOK := nameToID[names[1]]
	if !srcOK || !dstOK {
		return nil
	}
	return []store.Edge{{Src: src, Dst: dst, Kind: store.EdgeCalls, Weight: 1}}
}

func TestIndexer_SkipsUnchangedFile(t *testing.T) {
	const (
		testPath = "testfile.go"
		testHash = "abc"
	)

	ms := &mockStore{
		files: []store.File{
			{ID: 1, Path: testPath, Language: "go", Hash: testHash},
		},
	}

	records := make(chan index.FileRecord, 1)
	errs := make(chan error, 1)
	records <- index.FileRecord{
		Path:     "/fake/root/" + testPath,
		RelPath:  testPath,
		Language: "go",
		Hash:     testHash,
	}
	close(records)
	close(errs)

	fakeWalker := index.NewFakeWalker(records, errs)
	noExtractor := func(_ string) (index.LanguageExtractor, bool) { return nil, false }
	idx := index.NewIndexer(ms, &mockParser{}, &mockEmbedder{}, fakeWalker, noExtractor, slog.Default())

	stats, err := idx.Index(context.Background(), "/fake/root")
	require.NoError(t, err)
	require.Equal(t, 1, stats.FilesSkipped)
	require.Equal(t, 0, stats.FilesIndexed)
	require.Equal(t, 0, ms.saveCalls, "SaveFile must not be called for unchanged file")
	require.Equal(t, 0, ms.symCalls, "SaveSymbolBatch must not be called for unchanged file")
}

func TestIndexer_EmbedsStructuredSymbolContext(t *testing.T) {
	root := t.TempDir()
	relPath := filepath.Join("internal", "demo.go")
	fullPath := filepath.Join(root, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
	require.NoError(t, os.WriteFile(fullPath, []byte("package internal\nfunc RunIndex() {}\n"), 0644))

	records := make(chan index.FileRecord, 1)
	errs := make(chan error, 1)
	records <- index.FileRecord{
		Path:     fullPath,
		RelPath:  relPath,
		Language: "go",
		Hash:     "new-hash",
	}
	close(records)
	close(errs)

	me := &mockEmbedder{}
	ms := &mockStore{}
	extractor := staticExtractor{symbols: []store.Symbol{{
		Name:          "RunIndex",
		Kind:          store.KindFunction,
		QualifiedName: "internal.RunIndex",
		StartLine:     2,
		EndLine:       4,
		Signature:     "func RunIndex(ctx context.Context) error",
		Docstring:     "RunIndex walks files and stores symbols.",
		BodyExcerpt:   "func RunIndex(ctx context.Context) error { return nil }",
	}}}

	idx := index.NewIndexer(ms, &mockParser{}, me, index.NewFakeWalker(records, errs), func(language string) (index.LanguageExtractor, bool) {
		require.Equal(t, "go", language)
		return extractor, true
	}, slog.Default())

	stats, err := idx.Index(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Symbols)
	require.Equal(t, store.CurrentIndexContentVersion, ms.contentVersion)
	require.Equal(t, []int64{1}, ms.embeddedIDs)
	require.Len(t, me.texts, 1)
	require.Contains(t, me.texts[0], "file: "+relPath)
	require.Contains(t, me.texts[0], "language: go")
	require.Contains(t, me.texts[0], "kind: function")
	require.Contains(t, me.texts[0], "internal run index - function in internal demo go")
	require.Contains(t, me.texts[0], "symbol: internal.RunIndex")
	require.Contains(t, me.texts[0], "signature: func RunIndex(ctx context.Context) error")
	require.Contains(t, me.texts[0], "RunIndex walks files and stores symbols.")
	require.Contains(t, me.texts[0], "body:\nfunc RunIndex(ctx context.Context) error { return nil }")
}

func TestIndexer_ExtractsEdgesAfterAllChangedFileSymbolsAreSaved(t *testing.T) {
	root := t.TempDir()
	callerRel := filepath.Join("pkg", "caller.go")
	calleeRel := filepath.Join("pkg", "callee.go")
	callerSource := []byte("package pkg\nfunc Caller() { Callee() }\n")
	calleeSource := []byte("package pkg\nfunc Callee() {}\n")
	for rel, source := range map[string][]byte{callerRel: callerSource, calleeRel: calleeSource} {
		fullPath := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		require.NoError(t, os.WriteFile(fullPath, source, 0644))
	}

	records := make(chan index.FileRecord, 2)
	errs := make(chan error, 1)
	records <- index.FileRecord{Path: filepath.Join(root, callerRel), RelPath: callerRel, Language: "go", Hash: "caller-hash"}
	records <- index.FileRecord{Path: filepath.Join(root, calleeRel), RelPath: calleeRel, Language: "go", Hash: "callee-hash"}
	close(records)
	close(errs)

	extractor := &sourceAwareExtractor{
		symbolsBySource: map[string][]store.Symbol{
			string(callerSource): {{Name: "Caller", Kind: store.KindFunction, QualifiedName: "pkg.Caller", BodyExcerpt: string(callerSource)}},
			string(calleeSource): {{Name: "Callee", Kind: store.KindFunction, QualifiedName: "pkg.Callee", BodyExcerpt: string(calleeSource)}},
		},
		edgeForNames: map[string][2]string{
			string(callerSource): {"pkg.Caller", "pkg.Callee"},
		},
	}
	ms := &mockStore{}

	idx := index.NewIndexer(ms, &mockParser{}, &mockEmbedder{}, index.NewFakeWalker(records, errs), func(language string) (index.LanguageExtractor, bool) {
		require.Equal(t, "go", language)
		return extractor, true
	}, slog.Default())

	stats, err := idx.Index(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Symbols)
	require.Equal(t, 1, stats.Edges)
	require.Len(t, ms.edges, 1)
	require.NotEqual(t, ms.edges[0].Src, ms.edges[0].Dst)
	require.Len(t, extractor.seenNameMaps, 2)
	require.Contains(t, extractor.seenNameMaps[0], "pkg.Caller")
	require.Contains(t, extractor.seenNameMaps[0], "pkg.Callee")
}

func TestIndexer_ExtractsEdgesToUnchangedFileSymbols(t *testing.T) {
	root := t.TempDir()
	callerRel := filepath.Join("pkg", "caller.go")
	calleeRel := filepath.Join("pkg", "callee.go")
	callerSource := []byte("package pkg\nfunc Caller() { Callee() }\n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, callerRel), callerSource, 0644))

	records := make(chan index.FileRecord, 2)
	errs := make(chan error, 1)
	records <- index.FileRecord{Path: filepath.Join(root, callerRel), RelPath: callerRel, Language: "go", Hash: "new-caller"}
	records <- index.FileRecord{Path: filepath.Join(root, calleeRel), RelPath: calleeRel, Language: "go", Hash: "same-callee"}
	close(records)
	close(errs)

	extractor := &sourceAwareExtractor{
		symbolsBySource: map[string][]store.Symbol{
			string(callerSource): {{Name: "Caller", Kind: store.KindFunction, QualifiedName: "pkg.Caller", BodyExcerpt: string(callerSource)}},
		},
		edgeForNames: map[string][2]string{
			string(callerSource): {"pkg.Caller", "pkg.Callee"},
		},
	}
	ms := &mockStore{
		files: []store.File{
			{ID: 10, Path: callerRel, Language: "go", Hash: "old-caller"},
			{ID: 20, Path: calleeRel, Language: "go", Hash: "same-callee"},
		},
		symbolsByLanguage: map[string][]store.Symbol{
			"go": {{ID: 200, FileID: 20, Name: "Callee", Kind: store.KindFunction, QualifiedName: "pkg.Callee", BodyExcerpt: "func Callee() {}"}},
		},
	}

	idx := index.NewIndexer(ms, &mockParser{}, &mockEmbedder{}, index.NewFakeWalker(records, errs), func(language string) (index.LanguageExtractor, bool) {
		require.Equal(t, "go", language)
		return extractor, true
	}, slog.Default())

	stats, err := idx.Index(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Edges)
	require.Len(t, ms.edges, 1)
	require.Equal(t, int64(200), ms.edges[0].Dst)
}

func TestIndexer_EdgeSourceUsesCurrentFileWhenQualifiedNameIsDuplicated(t *testing.T) {
	root := t.TempDir()
	callerSource := []byte("package main\nfunc main() { scrape.NewManager() }\n")
	otherMainSource := []byte("package main\nfunc main() {}\n")
	targetSource := []byte("package scrape\nfunc NewManager() {}\n")
	records := make(chan index.FileRecord, 3)
	errs := make(chan error, 1)
	for i, item := range []struct {
		rel    string
		source []byte
	}{
		{rel: filepath.Join("cmd", "prometheus", "main.go"), source: callerSource},
		{rel: filepath.Join("tools", "generator", "main.go"), source: otherMainSource},
		{rel: filepath.Join("scrape", "manager.go"), source: targetSource},
	} {
		fullPath := filepath.Join(root, item.rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		require.NoError(t, os.WriteFile(fullPath, item.source, 0644))
		records <- index.FileRecord{Path: fullPath, RelPath: item.rel, Language: "go", Hash: fmt.Sprintf("hash-%d", i)}
	}
	close(records)
	close(errs)

	extractor := &sourceAwareExtractor{
		symbolsBySource: map[string][]store.Symbol{
			string(callerSource):    {{Name: "main", Kind: store.KindFunction, QualifiedName: "main.main"}},
			string(otherMainSource): {{Name: "main", Kind: store.KindFunction, QualifiedName: "main.main"}},
			string(targetSource):    {{Name: "NewManager", Kind: store.KindFunction, QualifiedName: "scrape.NewManager"}},
		},
		edgeForNames: map[string][2]string{
			string(callerSource): {"main.main", "scrape.NewManager"},
		},
	}
	ms := &mockStore{}
	idx := index.NewIndexer(ms, &mockParser{}, &mockEmbedder{}, index.NewFakeWalker(records, errs), func(string) (index.LanguageExtractor, bool) {
		return extractor, true
	}, slog.Default())

	stats, err := idx.Index(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Edges)
	require.Equal(t, []store.Edge{{Src: 1, Dst: 3, Kind: store.EdgeCalls, Weight: 1}}, ms.edges)
}

func TestIndexer_SkipsAmbiguousCalleeQualifiedNameAcrossFiles(t *testing.T) {
	root := t.TempDir()
	callerSource := []byte("package caller\nfunc Run() { dup.Helper() }\n")
	helperOneSource := []byte("package dup\nfunc Helper() {}\n")
	helperTwoSource := []byte("package dup\nfunc Helper() {}\n")
	records := make(chan index.FileRecord, 3)
	errs := make(chan error, 1)
	for i, item := range []struct {
		rel    string
		source []byte
	}{
		{rel: filepath.Join("caller", "caller.go"), source: callerSource},
		{rel: filepath.Join("one", "helper.go"), source: helperOneSource},
		{rel: filepath.Join("two", "helper.go"), source: helperTwoSource},
	} {
		fullPath := filepath.Join(root, item.rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		require.NoError(t, os.WriteFile(fullPath, item.source, 0644))
		records <- index.FileRecord{Path: fullPath, RelPath: item.rel, Language: "go", Hash: fmt.Sprintf("hash-%d", i)}
	}
	close(records)
	close(errs)

	extractor := &sourceAwareExtractor{
		symbolsBySource: map[string][]store.Symbol{
			string(callerSource):    {{Name: "Run", Kind: store.KindFunction, QualifiedName: "caller.Run"}},
			string(helperOneSource): {{Name: "Helper", Kind: store.KindFunction, QualifiedName: "dup.Helper"}},
			string(helperTwoSource): {{Name: "Helper", Kind: store.KindFunction, QualifiedName: "dup.Helper"}},
		},
		edgeForNames: map[string][2]string{
			string(callerSource): {"caller.Run", "dup.Helper"},
		},
	}
	ms := &mockStore{}
	idx := index.NewIndexer(ms, &mockParser{}, &mockEmbedder{}, index.NewFakeWalker(records, errs), func(string) (index.LanguageExtractor, bool) {
		return extractor, true
	}, slog.Default())

	stats, err := idx.Index(context.Background(), root)
	require.NoError(t, err)
	require.Zero(t, stats.Edges)
	require.Empty(t, ms.edges)
}

func TestIndexer_ReusesEmbeddingForUnchangedSymbolTextInChangedFile(t *testing.T) {
	root := t.TempDir()
	relPath := filepath.Join("pkg", "service.go")
	fullPath := filepath.Join(root, relPath)
	source := []byte("package pkg\nfunc Stable() {}\nfunc Changed() { println(1) }\n")
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
	require.NoError(t, os.WriteFile(fullPath, source, 0644))

	records := make(chan index.FileRecord, 1)
	errs := make(chan error, 1)
	records <- index.FileRecord{Path: fullPath, RelPath: relPath, Language: "go", Hash: "new-hash"}
	close(records)
	close(errs)

	stableOld := store.Symbol{ID: 50, FileID: 10, Name: "Stable", Kind: store.KindFunction, QualifiedName: "pkg.Stable", BodyExcerpt: "func Stable() {}"}
	extractor := staticExtractor{symbols: []store.Symbol{
		{Name: "Stable", Kind: store.KindFunction, QualifiedName: "pkg.Stable", BodyExcerpt: "func Stable() {}"},
		{Name: "Changed", Kind: store.KindFunction, QualifiedName: "pkg.Changed", BodyExcerpt: "func Changed() { println(1) }"},
	}}
	me := &mockEmbedder{}
	ms := &mockStore{
		files:         []store.File{{ID: 10, Path: relPath, Language: "go", Hash: "old-hash"}},
		symbolsByFile: map[int64][]store.Symbol{10: {stableOld}},
		embeddings:    map[int64][]float32{50: make([]float32, 384)},
	}

	idx := index.NewIndexer(ms, &mockParser{}, me, index.NewFakeWalker(records, errs), func(language string) (index.LanguageExtractor, bool) {
		require.Equal(t, "go", language)
		return extractor, true
	}, slog.Default())

	stats, err := idx.Index(context.Background(), root)
	require.NoError(t, err)
	require.Equal(t, 2, stats.Symbols)
	require.Len(t, me.texts, 1, "only changed symbol should be embedded")
	require.Contains(t, me.texts[0], "symbol: pkg.Changed")
	require.Equal(t, []int64{1, 2}, ms.embeddedIDs, "reused and newly embedded vectors should both be upserted")
}
