package sqlite

import (
	"context"
	"fmt"
	"sync"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// graphCache keeps the text-free symbol meta and the full edge list in RAM.
// Profiling showed every query re-reading both tables (90K meta rows + 99K
// edges ≈ 500ms warm on cockroach) for graph-context enrichment and the PPR
// graph build. Unlike the vector cache there is no sidecar file: a cold load
// is one sequential scan (~0.3s once per process), not worth invalidation
// surface. Mutations invalidate it through the same hooks as the vec cache.
//
// Callers MUST treat the returned slices as immutable.
type graphCache struct {
	mu          sync.RWMutex
	symsLoaded  bool
	syms        []store.Symbol
	edgesLoaded bool
	edges       []store.Edge
}

func (s *Store) invalidateGraphCache() {
	s.graph.mu.Lock()
	s.graph.symsLoaded = false
	s.graph.syms = nil
	s.graph.edgesLoaded = false
	s.graph.edges = nil
	s.graph.mu.Unlock()
}

// ListSymbolMeta returns every symbol's identity and location WITHOUT the
// text columns (signature/docstring/body_excerpt), served from RAM after the
// first call. The full-table hydration this replaced churned ~180MB per
// query on a 90K-symbol index.
func (s *Store) ListSymbolMeta(ctx context.Context) ([]store.Symbol, error) {
	s.graph.mu.RLock()
	if s.graph.symsLoaded {
		syms := s.graph.syms
		s.graph.mu.RUnlock()
		return syms, nil
	}
	s.graph.mu.RUnlock()

	s.graph.mu.Lock()
	defer s.graph.mu.Unlock()
	if s.graph.symsLoaded {
		return s.graph.syms, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id,file_id,name,kind,qualified_name,start_line,end_line FROM symbols`)
	if err != nil {
		return nil, fmt.Errorf("list symbol meta: %w", err)
	}
	defer rows.Close()
	var syms []store.Symbol
	for rows.Next() {
		var sym store.Symbol
		if err := rows.Scan(&sym.ID, &sym.FileID, &sym.Name, &sym.Kind, &sym.QualifiedName,
			&sym.StartLine, &sym.EndLine); err != nil {
			return nil, fmt.Errorf("list symbol meta scan: %w", err)
		}
		syms = append(syms, sym)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.graph.syms = syms
	s.graph.symsLoaded = true
	return syms, nil
}

// ListAllEdges returns the whole edge table, served from RAM after the first
// call (the PPR graph build and graph-context enrichment both consume it on
// every query).
func (s *Store) ListAllEdges(ctx context.Context) ([]store.Edge, error) {
	s.graph.mu.RLock()
	if s.graph.edgesLoaded {
		edges := s.graph.edges
		s.graph.mu.RUnlock()
		return edges, nil
	}
	s.graph.mu.RUnlock()

	s.graph.mu.Lock()
	defer s.graph.mu.Unlock()
	if s.graph.edgesLoaded {
		return s.graph.edges, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT src,dst,kind,weight FROM edges`)
	if err != nil {
		return nil, fmt.Errorf("list all edges: %w", err)
	}
	defer rows.Close()
	var edges []store.Edge
	for rows.Next() {
		var e store.Edge
		if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &e.Weight); err != nil {
			return nil, fmt.Errorf("list all edges scan: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.graph.edges = edges
	s.graph.edgesLoaded = true
	return edges, nil
}
