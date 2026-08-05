package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/enrich"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

// RunEnrich generates synthetic purpose docstrings for indexed symbols via an
// OpenAI-compatible LLM endpoint and stores them in
// <root>/.contextmaxxer/synthetic-docs.json. Run `index` afterwards (with
// -force to re-embed unchanged files) so the summaries reach the embeddings.
func RunEnrich(ctx context.Context, args []string, log *slog.Logger, modelName string) error {
	fs := flag.NewFlagSet("enrich", flag.ContinueOnError)
	llmURL := fs.String("llm-url", envOr("CONTEXTMAXXER_LLM_URL", "http://localhost:11434/v1"), "OpenAI-compatible chat endpoint (Ollama, LM Studio, DeepSeek, ...)")
	llmModel := fs.String("llm-model", envOr("CONTEXTMAXXER_LLM_MODEL", "qwen2.5-coder:3b"), "model name at the endpoint")
	llmKey := fs.String("llm-key", os.Getenv("CONTEXTMAXXER_LLM_KEY"), "API key (empty for local backends)")
	concurrency := fs.Int("concurrency", 2, "parallel LLM requests (keep low for local backends)")
	limit := fs.Int("limit", 0, "max symbols to enrich this run (0 = all)")
	force := fs.Bool("force", false, "regenerate summaries that already exist in the sidecar")
	minLines := fs.Int("min-lines", 3, "skip symbols whose body spans fewer lines")
	emitPending := fs.Bool("emit-pending", false, "write symbols needing summaries to pending-docs.json instead of calling an LLM (host-agent flow: MCP clients do not support sampling, so the agent fills the sidecar itself)")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := "."
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	dbPath := filepath.Join(absRoot, ".contextmaxxer", "index.db")
	sidecarPath := filepath.Join(absRoot, ".contextmaxxer", "synthetic-docs.json")

	// Dim is irrelevant for reading symbols, but sqlite.New needs the embedding
	// table config; reuse the standard model spec via the indexer default.
	st, err := sqlite.New(dbPath, modelName, 768)
	if err != nil {
		return fmt.Errorf("open index (run `contextmaxxer index` first): %w", err)
	}
	defer st.Close()

	ids, err := st.ListAllSymbolIDs(ctx)
	if err != nil {
		return fmt.Errorf("list symbols: %w", err)
	}
	syms, err := st.GetSymbolsByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("load symbols: %w", err)
	}
	fileIDSet := map[int64]bool{}
	for _, s := range syms {
		fileIDSet[s.FileID] = true
	}
	fileIDs := make([]int64, 0, len(fileIDSet))
	for id := range fileIDSet {
		fileIDs = append(fileIDs, id)
	}
	filePaths, err := st.GetFilesByIDs(ctx, fileIDs)
	if err != nil {
		return fmt.Errorf("load file paths: %w", err)
	}

	sidecar := map[string]string{}
	if data, err := os.ReadFile(sidecarPath); err == nil {
		if err := json.Unmarshal(data, &sidecar); err != nil {
			return fmt.Errorf("parse existing %s: %w", sidecarPath, err)
		}
	}

	enrichable := []string{"function", "method", "class"}
	var todo []store.Symbol
	for _, s := range syms {
		if !contains(enrichable, s.Kind) {
			continue
		}
		if s.EndLine-s.StartLine < *minLines {
			continue
		}
		if _, exists := sidecar[s.QualifiedName]; exists && !*force {
			continue
		}
		todo = append(todo, s)
	}
	// Deterministic order: by file then line, so partial runs are predictable.
	sort.Slice(todo, func(i, j int) bool {
		if todo[i].FileID != todo[j].FileID {
			return filePaths[todo[i].FileID] < filePaths[todo[j].FileID]
		}
		return todo[i].StartLine < todo[j].StartLine
	})
	if *limit > 0 && len(todo) > *limit {
		todo = todo[:*limit]
	}

	fmt.Printf("Symbols:    %4d total, %d already summarized, %d to enrich\n", len(syms), len(sidecar), len(todo))
	if len(todo) == 0 {
		return nil
	}

	if *emitPending {
		type pendingItem struct {
			QualifiedName string `json:"qualified_name"`
			Kind          string `json:"kind"`
			File          string `json:"file"`
			Signature     string `json:"signature,omitempty"`
			Docstring     string `json:"docstring,omitempty"`
			BodyExcerpt   string `json:"body_excerpt,omitempty"`
		}
		items := make([]pendingItem, 0, len(todo))
		for _, sym := range todo {
			body := sym.BodyExcerpt
			if len(body) > 1000 {
				body = body[:1000] + " ..."
			}
			items = append(items, pendingItem{
				QualifiedName: sym.QualifiedName,
				Kind:          sym.Kind,
				File:          filePaths[sym.FileID],
				Signature:     sym.Signature,
				Docstring:     sym.Docstring,
				BodyExcerpt:   body,
			})
		}
		pendingPath := filepath.Join(absRoot, ".contextmaxxer", "pending-docs.json")
		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal pending docs: %w", err)
		}
		if err := os.WriteFile(pendingPath, data, 0644); err != nil {
			return fmt.Errorf("write pending docs: %w", err)
		}
		fmt.Printf("Pending:    %4d symbols -> %s\n", len(items), pendingPath)
		fmt.Printf("Next:       have the host agent write one-sentence purpose summaries for each\n")
		fmt.Printf("            qualified_name into %s,\n", sidecarPath)
		fmt.Printf("            then run: contextmaxxer -force index %s\n", root)
		return nil
	}

	fmt.Printf("LLM:        %s @ %s (concurrency %d)\n", *llmModel, *llmURL, *concurrency)

	client := enrich.NewClient(enrich.Config{
		BaseURL: *llmURL,
		Model:   *llmModel,
		APIKey:  *llmKey,
		Log:     log,
	})

	var (
		mu     sync.Mutex
		done   int
		failed int
		start  = time.Now()
	)
	saveSidecar := func() error {
		mu.Lock()
		defer mu.Unlock()
		return writeSidecar(sidecarPath, sidecar)
	}

	if *concurrency < 1 {
		*concurrency = 1
	}
	jobs := make(chan store.Symbol)
	var wg sync.WaitGroup
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sym := range jobs {
				summary, err := client.SummarizeSymbol(ctx, filePaths[sym.FileID], sym)
				mu.Lock()
				if err != nil {
					failed++
					log.Warn("enrich failed", "symbol", sym.QualifiedName, "err", err)
				} else {
					sidecar[sym.QualifiedName] = summary
					done++
				}
				progress := done + failed
				mu.Unlock()
				if progress%25 == 0 {
					_ = saveSidecar()
					elapsed := time.Since(start).Round(time.Second)
					fmt.Printf("  %4d/%d (%d failed, %v elapsed)\n", progress, len(todo), failed, elapsed)
				}
			}
		}()
	}

dispatch:
	for _, sym := range todo {
		select {
		case <-ctx.Done():
			break dispatch
		case jobs <- sym:
		}
	}
	close(jobs)
	wg.Wait()

	if err := saveSidecar(); err != nil {
		return fmt.Errorf("write sidecar: %w", err)
	}
	fmt.Printf("Enriched:   %4d symbols (%d failed) in %v -> %s\n", done, failed, time.Since(start).Round(time.Second), sidecarPath)
	fmt.Printf("Next:       contextmaxxer -force index %s   (re-embeds with the new summaries)\n", root)
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func writeSidecar(path string, sidecar map[string]string) error {
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return strings.TrimSpace(fallback)
}
