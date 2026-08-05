package embed

import "context"

// DECISION: ZeroEmbedder is kept for --no-embeddings flag and unit tests that don't need the model.
type ZeroEmbedder struct {
	dim int
}

func NewZeroEmbedder() *ZeroEmbedder { return &ZeroEmbedder{dim: 384} }
func NewZeroEmbedderWithDim(dim int) *ZeroEmbedder {
	if dim <= 0 {
		dim = 384
	}
	return &ZeroEmbedder{dim: dim}
}
func (z *ZeroEmbedder) Dimension() int { return z.dim }
func (z *ZeroEmbedder) Close() error   { return nil }
func (z *ZeroEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, z.dim)
	}
	return out, nil
}
