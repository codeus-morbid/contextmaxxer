package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
)

// RunWarmup downloads and initializes the embedding model, reranker and ONNX
// runtime up front, so the first real `init`/`index`/`mcp` run starts instantly
// instead of appearing to hang on a ~250MB download. Idempotent: a no-op once
// everything is cached.
func RunWarmup(ctx context.Context, _ []string, log *slog.Logger, modelName string) error {
	if modelName == "" {
		modelName = "jina-embeddings-v2-base-code"
	}
	fmt.Println("Preparing contextmaxxer. The first run downloads the embedding model,")
	fmt.Println("reranker and ONNX runtime (~250MB, one-time) — this can take a few")
	fmt.Println("minutes on a slow connection. Progress is logged below.")

	start := time.Now()

	fmt.Printf("\n[1/2] embedding model: %s\n", modelName)
	embedder, err := embed.NewOnnxEmbedder(ctx, embed.Config{ModelName: modelName, BatchSize: 1, Log: log})
	if err != nil {
		return fmt.Errorf("prepare embedder: %w", err)
	}
	defer embedder.Close()
	if _, err := embedder.Embed(ctx, []string{"warmup"}); err != nil {
		return fmt.Errorf("embedder self-test: %w", err)
	}

	fmt.Printf("\n[2/2] reranker: %s\n", rerank.JinaRerankerV1TinyEN)
	_, closer, err := rerank.New(ctx, rerank.Config{ModelName: rerank.JinaRerankerV1TinyEN, Log: log})
	if err != nil {
		return fmt.Errorf("prepare reranker: %w", err)
	}
	if closer != nil {
		defer closer.Close()
	}

	fmt.Printf("\nReady in %v. Models are cached; subsequent runs start offline.\n", time.Since(start).Round(time.Second))
	return nil
}
