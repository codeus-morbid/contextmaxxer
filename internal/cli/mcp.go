package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/index/languages"
	mcpsrv "github.com/codeus-morbid/contextmaxxer/internal/mcp"
	"github.com/codeus-morbid/contextmaxxer/internal/releasecfg"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

func RunMCP(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	indexPath := fs.String("index", "./.contextmaxxer/index.db", "path to index db")
	// CONTEXTMAXXER_MODEL matches the global flag's behaviour in internal/app,
	// so a harness that spawns this server can pin the embedder the same way
	// the index was built — a mismatch is caught by the store, but only after
	// the run, which wastes the run.
	defaultModel := "jina-embeddings-v2-base-code"
	if env := os.Getenv("CONTEXTMAXXER_MODEL"); env != "" {
		defaultModel = env
	}
	modelName := fs.String("model", defaultModel, "embedding model name")
	// DECISION(2026-06): default to the tiny cross-encoder — it is the
	// holdout-validated release config (+0.36 Hit@1 over no-rerank, ~200ms) and
	// what the README documents. "none" was the old default and silently shipped
	// degraded ranking. ASSUMES: ~30MB model download is acceptable on first run.
	rerankerName := fs.String("reranker", releasecfg.Reranker, "reranker model: jina-reranker-v1-tiny-en (default), jina-reranker-v2-base-multilingual, or none")
	escalateName := fs.String("escalate-reranker", rerank.NoneName, "stronger reranker applied only to low-confidence queries")
	// Default true: part of the measured release config ("tiny + intent +
	// adaptive"); hand-rolled setups without the flag were getting an
	// unbenchmarked variant.
	adaptiveRerank := fs.Bool("adaptive-rerank", releasecfg.AdaptiveRerank, "skip reranker for confident exact/constructor top matches")
	lazyRerank := fs.Bool("lazy-rerank", false, "run the cross-encoder only when the fused ranking is ambiguous")
	watch := fs.Bool("watch", false, "watch the project for source changes and incrementally reindex in the background (keeps the index fresh while you code)")
	// DECISION(2026-06): feedback recording is ON by default — query+result
	// history is the training data for usage-priors and a learned ranker later;
	// it costs nothing to collect and cannot be backfilled.
	feedbackLog := fs.String("feedback-log", "./.contextmaxxer/feedback.jsonl", "path to feedback JSONL log; \"none\" disables recording")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	spec, err := embed.GetModel(*modelName)
	if err != nil {
		return fmt.Errorf("model: %w", err)
	}

	st, err := sqlite.New(*indexPath, *modelName, spec.Dim)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	embedder, err := embed.NewOnnxEmbedder(ctx, embed.Config{
		ModelName: *modelName,
		Log:       log,
	})
	if err != nil {
		return fmt.Errorf("init embedder: %w", err)
	}
	defer embedder.Close()

	rr, closer, err := rerank.New(ctx, rerank.Config{
		ModelName: *rerankerName,
		Log:       log,
	})
	if err != nil {
		return fmt.Errorf("init reranker: %w", err)
	}
	if closer != nil {
		defer closer.Close()
	}

	escalator, escCloser, err := rerank.New(ctx, rerank.Config{
		ModelName: *escalateName,
		Log:       log,
	})
	if err != nil {
		return fmt.Errorf("init escalation reranker: %w", err)
	}
	if escCloser != nil {
		defer escCloser.Close()
	}

	feedbackPath := *feedbackLog
	if feedbackPath == "none" {
		feedbackPath = ""
	}

	// The embedder is shared between the query path and (when --watch) the
	// background reindexer; serialize it since ONNX sessions are not concurrent-safe.
	safeEmbedder := embed.Synchronized(embedder)

	// The intent ranker is part of the RELEASE config every published gen-eval
	// number was measured with ("tiny + intent + adaptive"), but the served
	// paths never wired it — users got a different ranking than we measured.
	// Caught by a live probe where a metrics struct outranked the deciding
	// method the intent ranker's kind priors exist to promote.
	r := releasecfg.NewRetriever(st, safeEmbedder, rr, log)
	if escalator != nil {
		r.SetEscalator(escalator)
	}

	if *watch {
		if err := startWatcher(ctx, st, safeEmbedder, *indexPath, log); err != nil {
			return fmt.Errorf("start watcher: %w", err)
		}
	}

	// Instant-on: a --fast index has symbols/graph/FTS but no vectors. Serve
	// immediately (FTS+graph seeding degrades gracefully) and backfill
	// embeddings in the background; queries pick up semantics when it lands.
	go func() {
		n, err := index.BackfillEmbeddings(ctx, st, safeEmbedder, log)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("background embedding backfill failed (will resume next start)", "err", err, "embedded", n)
			}
			return
		}
		if n > 0 {
			if err := st.RebuildVecCacheFile(ctx); err != nil {
				log.Warn("vec cache rebuild after backfill failed", "err", err)
			}
			log.Info("embedding backfill complete", "embedded", n)
		}
	}()

	srv := mcpsrv.NewServerWithOptions(r, feedback.NewRecorder(feedbackPath), mcpsrv.Options{AdaptiveRerank: *adaptiveRerank, LazyRerank: *lazyRerank}, log)
	return srv.Serve(ctx)
}

// startWatcher spins up a background incremental reindexer over the SAME store
// the retriever reads from (so vec-cache invalidation on upsert reaches the
// query path) using the SAME synchronized embedder. The project root is derived
// from the index path: <root>/.contextmaxxer/index.db.
func startWatcher(ctx context.Context, st *sqlite.Store, embedder embed.Embedder, indexPath string, log *slog.Logger) error {
	absIndex, err := filepath.Abs(indexPath)
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(absIndex)) // strip index.db and .contextmaxxer

	parser, err := index.NewTreeSitterParser()
	if err != nil {
		return err
	}
	walker := index.NewWalkerWithConfig(log, index.WalkerConfig{})
	idx := index.NewIndexer(st, parser, embedder, walker, languages.New, log)

	overlayPath := filepath.Join(root, ".contextmaxxer", "synthetic-docs.json")
	if data, err := os.ReadFile(overlayPath); err == nil {
		var overlay map[string]string
		if json.Unmarshal(data, &overlay) == nil && len(overlay) > 0 {
			idx.SetDocOverlay(overlay)
		}
	}

	reindex := func(c context.Context) error {
		stats, err := idx.Index(c, root)
		if err != nil {
			return err
		}
		if stats.FilesIndexed > 0 || stats.FilesDeleted > 0 {
			log.Info("reindexed", "files", stats.FilesIndexed, "deleted", stats.FilesDeleted, "symbols", stats.Symbols)
		}
		return nil
	}

	go func() {
		defer parser.Close()
		// Catch-up pass: the event watcher only sees changes made after it
		// starts, so without this an index that went stale while the server was
		// down (files added/edited offline) would never reconcile. Incremental
		// hash-skip makes this a near no-op when the index is already current.
		if err := reindex(ctx); err != nil && ctx.Err() == nil {
			log.Warn("startup catch-up reindex failed", "err", err)
		}
		if err := index.Watch(ctx, root, reindex, log, 1500*time.Millisecond); err != nil && ctx.Err() == nil {
			log.Warn("watcher stopped", "err", err)
		}
	}()
	return nil
}
