package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
	"github.com/codeus-morbid/contextmaxxer/internal/releasecfg"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

func RunQuery(ctx context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	budget := fs.Int("budget", 0, "token budget (0 = default 32k)")
	// DECISION(2026-07): default 10, was 30. On the 144-case gen corpus the
	// first hit NEVER lands past rank 10 (Hit@10 = Hit@30 = 0.951) — ranks
	// 11-30 were pure token overhead.
	results := fs.Int("results", 10, "max results")
	indexPath := fs.String("index", "./.contextmaxxer/index.db", "path to index db")
	alphaFlag := fs.Float64("alpha", 0.7, "seed vs PPR weight (0=pure PPR, 1=pure seeds)")
	includeTrivial := fs.Bool("include-trivial", false, "include micro-symbols (single-line consts/vars) in results")
	modelName := fs.String("model", "jina-embeddings-v2-base-code", "embedding model name")
	rerankerName := fs.String("reranker", releasecfg.Reranker, "reranker model: jina-reranker-v1-tiny-en (default), jina-reranker-v2-base-multilingual, or none")
	rerankK := fs.Int("rerank-k", releasecfg.RerankK, "candidate count to rerank before returning results")
	adaptiveRerank := fs.Bool("adaptive-rerank", releasecfg.AdaptiveRerank, "skip reranker for confident exact/constructor top matches")
	feedbackLog := fs.String("feedback-log", "", "optional path to write retrieval feedback JSONL")
	modeFlag := fs.String("mode", "answer", "output mode: answer (default), minimal, explore")
	fs.SetOutput(os.Stderr)
	// Go's flag package stops at the first positional, so `query "text" --index x`
	// (text before flags, as the README showed) would silently ignore the flags.
	// Parse in a loop, collecting positionals, to accept flags in any position.
	alphaSet := false
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}

	// AlphaSet=true unconditionally disabled the paraphrastic protect fix
	// (unconditional top vector seeds in the rerank pool) on the CLI path —
	// eval and MCP both run with it. Only a user-provided -alpha opts out.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "alpha" {
			alphaSet = true
		}
	})

	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		return fmt.Errorf("usage: query [flags] <query text>")
	}

	spec, err := embed.GetModel(*modelName)
	if err != nil {
		return fmt.Errorf("model: %w", err)
	}

	// The default index path is cwd-relative, so running from the wrong
	// directory silently queries a DIFFERENT project's index and returns
	// plausible-looking results (caught by an agent-install dry run). Always
	// say which index answered.
	if abs, err := filepath.Abs(*indexPath); err == nil {
		fmt.Fprintf(os.Stderr, "index: %s\n", abs)
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

	outputMode, err := retrieve.ParseOutputMode(*modeFlag)
	if err != nil {
		return fmt.Errorf("mode: %w", err)
	}

	// Intent ranker on by default — matches the measured release config (see
	// the note in mcp.go).
	r := releasecfg.NewRetriever(st, embedder, rr, log)
	result, err := r.Retrieve(ctx, retrieve.Request{
		Query:          query,
		BudgetTokens:   *budget,
		MaxResults:     *results,
		RerankK:        *rerankK,
		AdaptiveRerank: *adaptiveRerank,
		Alpha:          float32(*alphaFlag),
		AlphaSet:       alphaSet,
		IncludeTrivial: *includeTrivial,
		OutputMode:     outputMode,
	})
	if err != nil {
		return fmt.Errorf("retrieve: %w", err)
	}
	requestID := feedback.NewRequestID()
	if rec := feedback.NewRecorder(*feedbackLog); rec != nil {
		candidates := make([]feedback.Candidate, len(result.Symbols))
		for i, sr := range result.Symbols {
			candidates[i] = feedback.Candidate{
				Rank:          i + 1,
				File:          sr.File,
				QualifiedName: sr.QualifiedName,
				Kind:          sr.Kind,
				Score:         sr.Score,
				Why:           sr.Why,
			}
		}
		if err := rec.RecordRetrieval(feedback.RetrievalEvent{
			RequestID:   requestID,
			Query:       query,
			Candidates:  candidates,
			TotalTokens: result.TotalTokens,
			Source:      "query",
		}); err != nil {
			return fmt.Errorf("record feedback: %w", err)
		}
	}

	if result.Structure != nil {
		fmt.Printf("=== structure: %s ===\n", result.Structure.Summary)
	}

	for _, sym := range result.Symbols {
		conf := sym.Confidence
		if conf == "" {
			conf = "-"
		}
		fmt.Printf("[%.4f|%s] %s\n", sym.Score, conf, sym.Why)
		fmt.Printf("  %s:%d-%d  %s (%s)\n", sym.File, sym.StartLine, sym.EndLine, sym.QualifiedName, sym.Kind)
		if outputMode != retrieve.OutputModeMinimal {
			if len(sym.Callers) > 0 {
				names := make([]string, len(sym.Callers))
				for i, c := range sym.Callers {
					names[i] = c.QualifiedName
				}
				fmt.Printf("  callers: %s\n", strings.Join(names, ", "))
			}
			if len(sym.Callees) > 0 {
				names := make([]string, len(sym.Callees))
				for i, c := range sym.Callees {
					names[i] = c.QualifiedName
				}
				fmt.Printf("  callees: %s\n", strings.Join(names, ", "))
			}
		}
		body := sym.Body
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		fmt.Printf("  %s\n\n", body)
	}

	if result.NextSteps != nil {
		ns := result.NextSteps
		if ns.IfTopCorrect != "" {
			fmt.Printf("=== next: %s ===\n", ns.IfTopCorrect)
		}
		if ns.IfUnsure != "" {
			fmt.Printf("=== unsure: %s ===\n", ns.IfUnsure)
		}
		for _, s := range ns.ToExplore {
			fmt.Printf("=== explore: %s ===\n", s)
		}
	}

	sts := result.Stats
	fmt.Printf("--- stats: seeds=%d nodes=%d edges=%d ppr_iter=%d embed=%dms graph=%dms ppr=%dms rerank=%dms total=%dms tokens=%d mode=%s ---\n",
		sts.SeedCount, sts.GraphNodes, sts.GraphEdges, sts.PPRIterations,
		sts.EmbedDuration.Milliseconds(), sts.GraphDuration.Milliseconds(),
		sts.PPRDuration.Milliseconds(), sts.RerankDuration.Milliseconds(), sts.Total.Milliseconds(),
		result.TotalTokens, outputMode.String(),
	)
	if *feedbackLog != "" {
		fmt.Printf("--- request_id=%s feedback_log=%s ---\n", requestID, *feedbackLog)
	}
	return nil
}
