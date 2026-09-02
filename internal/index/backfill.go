package index

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// backfillStore is the store slice BackfillEmbeddings needs (satisfied by
// *sqlite.Store; kept narrow so tests can fake it).
type backfillStore interface {
	ListSymbolsMissingEmbedding(ctx context.Context) ([]store.SymbolToEmbed, error)
	UpsertEmbeddingBatch(ctx context.Context, embeddings []store.Embedding) error
}

// graphNeighbourStore is the optional extra a store can offer so backfill can
// fold call-graph neighbours into the embedded text. Optional because the
// interface above is deliberately narrow and fakes implement only what they
// need; a store without it simply embeds the symbol on its own.
type graphNeighbourStore interface {
	ListAllEdges(ctx context.Context) ([]store.Edge, error)
	ListSymbolMeta(ctx context.Context) ([]store.Symbol, error)
}

// neighbourNames maps each symbol to the qualified names it is connected to,
// in both directions: a caller describes a symbol as usefully as a callee.
func neighbourNames(ctx context.Context, st backfillStore) map[int64][]string {
	// DECISION(2026-09): off unless CONTEXTMAXXER_GRAPH_CONTEXT is set. The idea
	// is sound — a symbol's callers name what it is for, and `resize()` has no
	// word in common with "avatar" until `uploadAvatar` is listed beside it —
	// but it did not survive measurement. Over 30 snapshots: HitFile 0.392 ->
	// 0.382 (paired mean -0.010, t = -1.36, CI crosses zero), better on zero
	// instances, worse on two, unchanged on 28. Precision moved +0.015 and the
	// first useful hit from 2.90 to 2.80, both within noise at this n.
	// A default is not changed on an unproven effect, so the code stays and the
	// behaviour does not.
	// REVISIT IF: a larger sample separates the precision gain from noise.
	if os.Getenv("CONTEXTMAXXER_GRAPH_CONTEXT") == "" {
		return nil
	}
	gs, ok := st.(graphNeighbourStore)
	if !ok {
		return nil
	}
	edges, err := gs.ListAllEdges(ctx)
	if err != nil || len(edges) == 0 {
		return nil
	}
	meta, err := gs.ListSymbolMeta(ctx)
	if err != nil {
		return nil
	}
	nameOf := make(map[int64]string, len(meta))
	for _, s := range meta {
		nameOf[s.ID] = s.QualifiedName
	}
	out := make(map[int64][]string, len(meta))
	add := func(id, other int64) {
		if n := nameOf[other]; n != "" && len(out[id]) < embedNeighborMax {
			out[id] = append(out[id], n)
		}
	}
	for _, e := range edges {
		add(e.Src, e.Dst)
		add(e.Dst, e.Src)
	}
	return out
}

// backfillChunk bounds one embed+upsert round. Each UpsertEmbeddingBatch
// invalidates the RAM vec cache, so the chunk is kept large enough that a
// concurrently-serving query path doesn't thrash reloads.
const backfillChunk = 128

// BackfillEmbeddings embeds every symbol that has no stored vector (the state
// a --fast structure-only index leaves behind) and upserts the results in
// chunks, so an interrupted run resumes where it stopped. Returns the number
// of symbols embedded.
func BackfillEmbeddings(ctx context.Context, st backfillStore, embedder embed.Embedder, log *slog.Logger) (int, error) {
	missing, err := st.ListSymbolsMissingEmbedding(ctx)
	if err != nil {
		return 0, err
	}
	if len(missing) == 0 {
		return 0, nil
	}
	// Neighbours are loaded once: the graph is the same for every chunk, and
	// re-reading every edge per chunk would dominate the backfill.
	neighbors := neighbourNames(ctx, st)
	log.Info("embedding backfill started", "symbols", len(missing), "with_graph_context", len(neighbors) > 0)

	done := 0
	for off := 0; off < len(missing); off += backfillChunk {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		end := off + backfillChunk
		if end > len(missing) {
			end = len(missing)
		}
		chunk := missing[off:end]

		texts := make([]string, len(chunk))
		for i, it := range chunk {
			texts[i] = EmbeddingTextWithNeighbors(it.Path, it.Language, it.Symbol, neighbors[it.ID])
		}
		vecs, err := embedder.Embed(ctx, texts)
		if err != nil {
			return done, fmt.Errorf("backfill embed [%d:%d]: %w", off, end, err)
		}
		embeddings := make([]store.Embedding, len(chunk))
		for i := range chunk {
			embeddings[i] = store.Embedding{SymbolID: chunk[i].ID, Vector: vecs[i]}
		}
		if err := st.UpsertEmbeddingBatch(ctx, embeddings); err != nil {
			return done, fmt.Errorf("backfill upsert [%d:%d]: %w", off, end, err)
		}
		done += len(chunk)
		log.Info("embedding backfill progress", "done", done, "total", len(missing))
	}
	return done, nil
}
