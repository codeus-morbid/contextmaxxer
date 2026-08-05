package embed

import "context"

type Embedder interface {
	Dimension() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Close() error
}

// queryEmbedder is the optional side of Embedder for asymmetric
// (instruction-tuned) models where queries get a different prefix than docs.
type queryEmbedder interface {
	EmbedQueries(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedQueries embeds query-side texts through e, using the model's query
// instruction prefix when the embedder supports it and falling back to plain
// Embed for symmetric models, fakes and the zero embedder. The parameter is
// the minimal Embed shape so callers' narrower embedder interfaces fit.
func EmbedQueries(ctx context.Context, e interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}, texts []string) ([][]float32, error) {
	if qe, ok := e.(queryEmbedder); ok {
		return qe.EmbedQueries(ctx, texts)
	}
	return e.Embed(ctx, texts)
}
