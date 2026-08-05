// Diagnostic tool: for selected (project, query_id) pairs, runs retrieval in
// both vector-only and hybrid modes with top-30 + features, and prints a
// compact side-by-side comparison highlighting the expected symbol's rank.
//
// Usage:
//
//	go run ./cmd/diag -manifest internal/eval/testdata/manifest.public.json \
//	    -targets "contextmaxxer:ctx_gen_effective_alpha" \
//	    -reranker jina-reranker-v2-base-multilingual -intent-ranker -adaptive-rerank
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/eval"
	"github.com/codeus-morbid/contextmaxxer/internal/intent"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

func main() {
	manifestPath := flag.String("manifest", "internal/eval/testdata/manifest.public.json", "manifest path")
	targets := flag.String("targets", "", "comma-separated project:case_id pairs")
	holdout := flag.Bool("holdout", false, "use holdout cases")
	rerankerName := flag.String("reranker", rerank.NoneName, "reranker model")
	rerankK := flag.Int("rerank-k", 15, "rerank K")
	maxResults := flag.Int("results", 10, "max results (eval default 10)")
	seedK := flag.Int("seed-k", 20, "seed K (eval default is 20)")
	adaptive := flag.Bool("adaptive-rerank", false, "adaptive rerank")
	intentR := flag.Bool("intent-ranker", false, "intent ranker")
	alpha := flag.Float64("alpha", 0.7, "alpha")
	modelName := flag.String("model", "jina-embeddings-v2-base-code", "embedding model")
	flag.Parse()

	if *targets == "" {
		fmt.Fprintln(os.Stderr, "must provide -targets project:case_id,project:case_id")
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	spec, err := embed.GetModel(*modelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "model: %v\n", err)
		os.Exit(1)
	}
	emb, err := embed.NewOnnxEmbedder(ctx, embed.Config{ModelName: *modelName})
	if err != nil {
		fmt.Fprintf(os.Stderr, "embedder: %v\n", err)
		os.Exit(1)
	}
	defer emb.Close()

	rr, closer, err := rerank.New(ctx, rerank.Config{ModelName: *rerankerName, Log: log})
	if err != nil {
		fmt.Fprintf(os.Stderr, "reranker: %v\n", err)
		os.Exit(1)
	}
	if closer != nil {
		defer closer.Close()
	}

	var ranker retrieve.Ranker
	if *intentR {
		ranker = intent.NewRanker()
	}

	manifest, err := eval.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "manifest: %v\n", err)
		os.Exit(1)
	}
	projByName := make(map[string]eval.ManifestProject, len(manifest.Projects))
	for _, p := range manifest.Projects {
		projByName[p.Name] = p
	}

	type target struct{ project, caseID string }
	var tgts []target
	for _, pair := range strings.Split(*targets, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			fmt.Fprintf(os.Stderr, "bad target %q (use project:case_id)\n", pair)
			os.Exit(2)
		}
		tgts = append(tgts, target{parts[0], parts[1]})
	}

	for _, t := range tgts {
		p, ok := projByName[t.project]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown project %q\n", t.project)
			continue
		}
		casesPath := p.CasesPath
		if *holdout {
			casesPath = p.CasesHoldoutPath
		}
		cases, err := eval.LoadCases(casesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load cases %s: %v\n", t.project, err)
			continue
		}
		var qc *eval.QueryCase
		for i := range cases {
			if cases[i].ID == t.caseID {
				qc = &cases[i]
				break
			}
		}
		if qc == nil {
			fmt.Fprintf(os.Stderr, "case %s not found in %s\n", t.caseID, t.project)
			continue
		}

		st, err := sqlite.New(p.IndexPath, *modelName, spec.Dim)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open index %s: %v\n", t.project, err)
			continue
		}
		r := retrieve.NewRetrieverWithRankers(st, emb, rr, ranker, log)

		req := func(mode retrieve.Mode) retrieve.Request {
			return retrieve.Request{
				Query:          qc.Query,
				BudgetTokens:   8000,
				SeedK:          *seedK,
				MaxResults:     *maxResults,
				RerankK:        *rerankK,
				AdaptiveRerank: *adaptive,
				Mode:           mode,
				Alpha:          float32(*alpha),
				AlphaSet:       true,
			}
		}

		hybRes, hErr := r.Retrieve(ctx, req(retrieve.ModeHybrid))
		vecRes, vErr := r.Retrieve(ctx, req(retrieve.ModeVectorOnly))
		st.Close()

		fmt.Printf("\n========================================================================\n")
		fmt.Printf("PROJECT %s | CASE %s\n", t.project, t.caseID)
		fmt.Printf("QUERY:    %s\n", qc.Query)
		fmt.Printf("EXPECTED: %s\n", strings.Join(qc.Expected, ", "))
		fmt.Printf("========================================================================\n")
		if hErr != nil {
			fmt.Printf("hybrid err: %v\n", hErr)
		}
		if vErr != nil {
			fmt.Printf("vector err: %v\n", vErr)
		}

		printSide(qc.Expected, hybRes.Symbols, vecRes.Symbols)
		printExpectedFeatures("HYBRID", qc.Expected, hybRes.Symbols, hybRes.Stats)
		printExpectedFeatures("VECTOR", qc.Expected, vecRes.Symbols, vecRes.Stats)
	}
}

func printSide(expected []string, hyb, vec []retrieve.ScoredResult) {
	expSet := make(map[string]bool, len(expected))
	for _, e := range expected {
		expSet[e] = true
	}
	n := 15
	if len(hyb) < n {
		n = len(hyb)
	}
	if len(vec) < n {
		n = len(vec)
	}
	fmt.Printf("\n  rank | %-50s | %-50s\n", "HYBRID (qn, score)", "VECTOR (qn, score)")
	fmt.Printf("  -----+-%-50s-+-%-50s\n", strings.Repeat("-", 50), strings.Repeat("-", 50))
	for i := 0; i < n; i++ {
		hStr, vStr := "", ""
		if i < len(hyb) {
			mark := " "
			if expSet[hyb[i].QualifiedName] {
				mark = "*"
			}
			hStr = fmt.Sprintf("%s%-44s %.3f", mark, trunc(hyb[i].QualifiedName, 44), hyb[i].Score)
		}
		if i < len(vec) {
			mark := " "
			if expSet[vec[i].QualifiedName] {
				mark = "*"
			}
			vStr = fmt.Sprintf("%s%-44s %.3f", mark, trunc(vec[i].QualifiedName, 44), vec[i].Score)
		}
		fmt.Printf("  %3d  | %-50s | %-50s\n", i+1, hStr, vStr)
	}
}

func printExpectedFeatures(mode string, expected []string, results []retrieve.ScoredResult, stats retrieve.Stats) {
	fmt.Printf("\n  --- %s expected-symbol diagnostics (eff_alpha=%.2f, seed_n=%d) ---\n", mode, stats.EffectiveAlpha, stats.SeedCount)
	for _, exp := range expected {
		idx := -1
		for i, s := range results {
			if s.QualifiedName == exp {
				idx = i
				break
			}
		}
		if idx < 0 {
			fmt.Printf("  [%s] NOT IN TOP-%d\n", exp, len(results))
			continue
		}
		s := results[idx]
		f := s.Features
		fmt.Printf("  [%s] final_rank=%d  score=%.3f  why=%s\n", exp, idx+1, s.Score, s.Why)
		fmt.Printf("    pre-rerank: SeedRank=%d  PPRRank=%d  SeedScore=%.3f  PPRScore=%.3f\n", f.SeedRank, f.PPRRank, f.SeedScore, f.PPRScore)
		fmt.Printf("    seed_signals: VectorSeed=%.3f  FTSSeed=%.3f  PPR=%.3f\n", f.VectorSeed, f.FTSSeed, f.PPR)
		fmt.Printf("    overlaps: short=%.2f  name=%.2f  path=%.2f  kind=%.2f  sig=%.2f  body=%.2f\n",
			f.ShortNameOverlap, f.NameOverlap, f.PathOverlap, f.KindOverlap, f.SignatureOverlap, f.BodyOverlap)
		fmt.Printf("    misc: ctor=%.1f  func=%.1f  method=%.1f  hub_penalty=%.2f\n",
			f.IsConstructor, f.KindFunction, f.KindMethod, f.HubPenalty)
	}
	// also show top-3 winners by SeedRank vs PPRRank for context
	if len(results) > 0 {
		preBySeed := append([]retrieve.ScoredResult(nil), results...)
		sort.Slice(preBySeed, func(i, j int) bool {
			ri, rj := preBySeed[i].Features.SeedRank, preBySeed[j].Features.SeedRank
			if ri == 0 {
				ri = 9999
			}
			if rj == 0 {
				rj = 9999
			}
			return ri < rj
		})
		var top3 []string
		for i := 0; i < 3 && i < len(preBySeed); i++ {
			top3 = append(top3, fmt.Sprintf("#%d %s", preBySeed[i].Features.SeedRank, trunc(preBySeed[i].QualifiedName, 35)))
		}
		fmt.Printf("    top-3 by SeedRank: %s\n", strings.Join(top3, " | "))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
