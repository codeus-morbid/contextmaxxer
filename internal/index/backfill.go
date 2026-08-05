package index

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// backfillStore is the store slice BackfillEmbeddings needs (satisfied by
// *sqlite.Store; kept narrow so tests can fake it).
type backfillStore interface {
	ListSymbolsMissingEmbedding(ctx context.Context) ([]store.SymbolToEmbed, error)
	UpsertEmbeddingBatch(ctx context.Context, embeddings []store.Embedding) error
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
	log.Info("embedding backfill started", "symbols", len(missing))

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
			texts[i] = EmbeddingText(it.Path, it.Language, it.Symbol)
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
