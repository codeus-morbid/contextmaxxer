package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/index/languages"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

func RunIndex(ctx context.Context, args []string, log *slog.Logger, indexPath string, noEmbeddings bool, includeTests bool, modelName string, forceReindex bool) error {
	root := "."
	if len(args) > 0 {
		root = args[0]
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	// A mistyped root (or a flag passed after the subcommand, which lands here
	// as a positional arg) must fail loudly, not "index" 0 files successfully.
	if fi, err := os.Stat(absRoot); err != nil || !fi.IsDir() {
		return fmt.Errorf("project root is not a directory: %s (flags go before the subcommand: contextmaxxer --fast index <path>)", absRoot)
	}

	if modelName == "" {
		modelName = "jina-embeddings-v2-base-code"
	}

	spec, err := embed.GetModel(modelName)
	if err != nil {
		return fmt.Errorf("model: %w", err)
	}

	// The global -index flag was silently ignored here until 2026-07 (this
	// path was hardcoded), which sent a benchmark script's output into the
	// production index. Relative paths resolve against the project root so the
	// default keeps its historical meaning; absolute paths are taken as-is.
	dbPath := indexPath
	if dbPath == "" {
		dbPath = filepath.Join(".contextmaxxer", "index.db")
	}
	if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(absRoot, dbPath)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("create index dir: %w", err)
	}

	st, err := sqlite.New(dbPath, modelName, spec.Dim)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	contentVersion, err := st.IndexContentVersion(ctx)
	if err != nil {
		return fmt.Errorf("inspect index content: %w", err)
	}
	if shouldForceReindex(forceReindex, contentVersion) {
		if !forceReindex {
			fmt.Printf("Index upgrade: content v%d -> v%d requires one full reindex\n", contentVersion, store.CurrentIndexContentVersion)
		}
		forceReindex = true
	}

	parser, err := index.NewTreeSitterParser()
	if err != nil {
		return fmt.Errorf("create parser: %w", err)
	}
	defer parser.Close()

	// DECISION(2026-07): structure-only mode passes a nil embedder (symbols get
	// NO vector rows) instead of the old ZeroEmbedder (all-zero vectors). Zero
	// rows poisoned vector search and made "not embedded yet" indistinguishable
	// from "embedded"; missing rows are what BackfillEmbeddings keys on.
	var embedder embed.Embedder
	if !noEmbeddings {
		e, err := embed.NewOnnxEmbedder(ctx, embed.Config{
			ModelName: modelName,
			Log:       log,
		})
		if err != nil {
			return fmt.Errorf("init embedder: %w", err)
		}
		defer e.Close()
		embedder = e
	}

	walker := index.NewWalkerWithConfig(log, index.WalkerConfig{IncludeTests: includeTests})
	idx := index.NewIndexer(st, parser, embedder, walker, languages.New, log)
	idx.SetForceReindex(forceReindex)

	overlayPath := filepath.Join(absRoot, ".contextmaxxer", "synthetic-docs.json")
	if data, err := os.ReadFile(overlayPath); err == nil {
		var overlay map[string]string
		if err := json.Unmarshal(data, &overlay); err != nil {
			log.Warn("parse synthetic-docs.json failed, ignoring", "err", err)
		} else if len(overlay) > 0 {
			idx.SetDocOverlay(overlay)
			fmt.Printf("Docstrings: %4d synthetic summaries loaded\n", len(overlay))
		}
	}

	stats, err := idx.Index(ctx, absRoot)
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}

	fmt.Printf("Indexed:   %4d files\n", stats.FilesIndexed)
	fmt.Printf("Skipped:   %4d files (unchanged)\n", stats.FilesSkipped)
	fmt.Printf("Deleted:   %4d files (removed from disk)\n", stats.FilesDeleted)
	fmt.Printf("Symbols:   %4d\n", stats.Symbols)
	fmt.Printf("Edges:     %4d\n", stats.Edges)
	fmt.Printf("Duration:  %v\n", stats.Duration.Round(10*1000*1000))

	if noEmbeddings {
		fmt.Println("Embeddings: skipped (--fast); run a plain index later or let the MCP server backfill them in the background")
		return nil
	}

	// A previous --fast run may have left unchanged files (hash-skipped above)
	// with no vectors; complete them now so a plain index always ends whole.
	if n, err := index.BackfillEmbeddings(ctx, st, embedder, log); err != nil {
		log.Warn("embedding backfill failed", "err", err)
	} else if n > 0 {
		fmt.Printf("Backfilled: %3d embeddings (from an earlier --fast index)\n", n)
	}

	// Pre-build the on-disk vec cache so the first query after indexing is fast
	// (no cold 90k-row load). Non-fatal: indexing already succeeded.
	if err := st.RebuildVecCacheFile(ctx); err != nil {
		log.Warn("vec cache pre-build failed (queries will build it lazily)", "err", err)
	}
	return nil
}

func shouldForceReindex(requested bool, contentVersion int) bool {
	return requested || contentVersion < store.CurrentIndexContentVersion
}
