package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// SaveSymbolChunks records the chunk rows for one symbol and returns their ids
// in order. Vectors are written separately, after embedding.
func (s *Store) SaveSymbolChunks(ctx context.Context, chunks []store.SymbolChunk) ([]int64, error) {
	if len(chunks) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("save chunks begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO symbol_chunks(symbol_id, start_line) VALUES(?,?)`)
	if err != nil {
		return nil, fmt.Errorf("save chunks prepare: %w", err)
	}
	defer stmt.Close()

	ids := make([]int64, len(chunks))
	for i, c := range chunks {
		res, err := stmt.ExecContext(ctx, c.SymbolID, c.StartLine)
		if err != nil {
			return nil, fmt.Errorf("save chunks exec: %w", err)
		}
		ids[i], _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("save chunks commit: %w", err)
	}
	return ids, nil
}

func (s *Store) UpsertChunkEmbeddingBatch(ctx context.Context, embeddings []store.ChunkEmbedding) error {
	if len(embeddings) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert chunk embeddings begin: %w", err)
	}
	defer tx.Rollback()

	delStmt, err := tx.PrepareContext(ctx, `DELETE FROM symbol_chunk_vec WHERE chunk_id=?`)
	if err != nil {
		return fmt.Errorf("upsert chunk embeddings delete prepare: %w", err)
	}
	defer delStmt.Close()
	insStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO symbol_chunk_vec(chunk_id, embedding) VALUES(?,?)`)
	if err != nil {
		return fmt.Errorf("upsert chunk embeddings insert prepare: %w", err)
	}
	defer insStmt.Close()

	for _, e := range embeddings {
		if _, err := delStmt.ExecContext(ctx, e.ChunkID); err != nil {
			return fmt.Errorf("upsert chunk embeddings delete: %w", err)
		}
		if _, err := insStmt.ExecContext(ctx, e.ChunkID, vectorJSON(e.Vector)); err != nil {
			return fmt.Errorf("upsert chunk embeddings insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert chunk embeddings commit: %w", err)
	}
	return nil
}

// SearchByChunkVector runs KNN over the chunk vectors and reports the owning
// symbols, best chunk first. A long function contributes many chunks, so the
// results are deduplicated by symbol — otherwise one sprawling function would
// fill the whole seed pool with its own windows.
func (s *Store) SearchByChunkVector(ctx context.Context, vec []float32, k int) ([]store.ScoredSymbol, error) {
	if k <= 0 {
		return nil, nil
	}
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	// Over-fetch: chunks of one symbol crowd the neighbourhood, and k distinct
	// symbols need more than k chunks.
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.symbol_id, knn.distance
		FROM (
			SELECT chunk_id, distance FROM symbol_chunk_vec WHERE embedding MATCH ? AND k = ?
			ORDER BY distance
		) AS knn
		JOIN symbol_chunks c ON c.id = knn.chunk_id
		ORDER BY knn.distance`, "["+strings.Join(parts, ",")+"]", k*4)
	if err != nil {
		return nil, fmt.Errorf("search by chunk vector: %w", err)
	}
	defer rows.Close()

	var ids []int64
	scores := make(map[int64]float32)
	for rows.Next() {
		var symbolID int64
		var distance float64
		if err := rows.Scan(&symbolID, &distance); err != nil {
			return nil, fmt.Errorf("search by chunk vector scan: %w", err)
		}
		if _, seen := scores[symbolID]; seen {
			continue
		}
		scores[symbolID] = 1.0 / (1.0 + float32(distance))
		ids = append(ids, symbolID)
		if len(ids) == k {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	syms, err := s.GetSymbolsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("hydrate chunk hits: %w", err)
	}
	byID := make(map[int64]store.Symbol, len(syms))
	for _, sym := range syms {
		byID[sym.ID] = sym
	}
	result := make([]store.ScoredSymbol, 0, len(ids))
	for _, id := range ids {
		sym, ok := byID[id]
		if !ok {
			continue // orphaned chunk; the symbol is gone
		}
		result = append(result, store.ScoredSymbol{Symbol: sym, Score: scores[id]})
	}
	return result, nil
}

// CountChunks reports how many chunk rows exist, for diagnostics and to tell an
// index that predates chunking from one that simply has no capped symbols.
func (s *Store) CountChunks(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM symbol_chunks`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count chunks: %w", err)
	}
	return n, nil
}
