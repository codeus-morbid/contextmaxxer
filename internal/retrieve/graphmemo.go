package retrieve

import (
	"context"
	"fmt"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type testEntry struct {
	shortName string
	ref       SymbolRef
}

// graphMemoData bundles the query-independent derived views of the symbol
// meta and edge tables: lookup maps, ID-keyed adjacency, file paths and the
// test-symbol index. Building them per query cost ~350ms of CPU on a
// 90K-symbol index even with the tables themselves served from RAM; the
// bundle is rebuilt only when the store hands out NEW slices (identity
// check), i.e. once per index generation.
type graphMemoData struct {
	syms        []store.Symbol
	edges       []store.Edge
	byQN        map[string]store.Symbol
	byID        map[int64]store.Symbol
	callerByID  map[int64][]int64
	calleeByID  map[int64][]int64
	callerEdges map[int64][]store.Edge
	calleeEdges map[int64][]store.Edge
	filePaths   map[int64]string
	testEntries []testEntry
}

// sameSliceIdentity reports whether two slices are the same backing array —
// the store's caches replace slices wholesale on invalidation, so identity
// is a correct and O(1) freshness check.
func sameSliceIdentity[T any](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

func (r *Retriever) getGraphMemo(ctx context.Context) (*graphMemoData, error) {
	edges, err := r.store.ListAllEdges(ctx)
	if err != nil {
		return nil, fmt.Errorf("list edges for enrichment: %w", err)
	}
	syms, err := r.store.ListSymbolMeta(ctx)
	if err != nil {
		return nil, fmt.Errorf("list symbol meta for enrichment: %w", err)
	}

	r.memoMu.Lock()
	defer r.memoMu.Unlock()
	if m := r.graphMemo; m != nil && sameSliceIdentity(m.syms, syms) && sameSliceIdentity(m.edges, edges) {
		return m, nil
	}

	m := &graphMemoData{
		syms:        syms,
		edges:       edges,
		byQN:        make(map[string]store.Symbol, len(syms)),
		byID:        make(map[int64]store.Symbol, len(syms)),
		callerByID:  make(map[int64][]int64),
		calleeByID:  make(map[int64][]int64),
		callerEdges: make(map[int64][]store.Edge),
		calleeEdges: make(map[int64][]store.Edge),
	}
	for _, sym := range syms {
		m.byQN[sym.QualifiedName] = sym
		m.byID[sym.ID] = sym
	}
	for _, e := range edges {
		m.callerByID[e.Dst] = append(m.callerByID[e.Dst], e.Src)
		m.calleeByID[e.Src] = append(m.calleeByID[e.Src], e.Dst)
		m.callerEdges[e.Dst] = append(m.callerEdges[e.Dst], e)
		m.calleeEdges[e.Src] = append(m.calleeEdges[e.Src], e)
	}

	fileIDs := make(map[int64]bool, len(syms)/8)
	for _, sym := range syms {
		fileIDs[sym.FileID] = true
	}
	fileIDList := make([]int64, 0, len(fileIDs))
	for id := range fileIDs {
		fileIDList = append(fileIDList, id)
	}
	m.filePaths, err = r.store.GetFilesByIDs(ctx, fileIDList)
	if err != nil {
		return nil, fmt.Errorf("get file paths for enrichment: %w", err)
	}

	for _, sym := range syms {
		fp := m.filePaths[sym.FileID]
		if !isTestFile(fp) {
			continue
		}
		short := sym.Name
		if short == "" {
			short = sym.QualifiedName
			if idx := strings.LastIndex(short, "."); idx >= 0 {
				short = short[idx+1:]
			}
		}
		m.testEntries = append(m.testEntries, testEntry{
			shortName: strings.ToLower(short),
			ref: SymbolRef{
				QualifiedName: sym.QualifiedName,
				File:          fp,
				Lines:         fmt.Sprintf("%d-%d", sym.StartLine, sym.EndLine),
				Kind:          sym.Kind,
			},
		})
	}

	r.graphMemo = m
	return m, nil
}
