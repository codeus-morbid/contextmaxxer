package embed

import (
	"context"
	"sync"
)

// synchronized serializes Embed calls on an underlying embedder. ONNX Runtime
// sessions are not safe for concurrent Run calls, so when the same embedder is
// shared between the MCP query path and a background reindex watcher, both must
// go through one lock. The lock is per-Embed-call (one batch), so queries still
// interleave between the watcher's batches rather than blocking for a whole
// reindex.
type synchronized struct {
	mu    sync.Mutex
	inner Embedder
}

// Synchronized wraps e so concurrent Embed calls are serialized. Dimension and
// Close delegate directly (Dimension is an immutable read; Close is called once
// at shutdown).
func Synchronized(e Embedder) Embedder {
	return &synchronized{inner: e}
}

func (s *synchronized) Dimension() int { return s.inner.Dimension() }

func (s *synchronized) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner.Embed(ctx, texts)
}

func (s *synchronized) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return EmbedQueries(ctx, s.inner, texts)
}

func (s *synchronized) Close() error { return s.inner.Close() }
