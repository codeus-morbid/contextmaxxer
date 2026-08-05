package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/eval"
	"github.com/codeus-morbid/contextmaxxer/internal/intent"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
	"log/slog"
)

func main() {
	os.Exit(run())
}

func run() int {
	indexPath := flag.String("index", ".contextmaxxer/index.db", "path to index db")
	casesPath := flag.String("cases", "internal/eval/testdata/queries.contextmaxxer.gen.json", "path to eval queries JSON")
	manifestPath := flag.String("manifest", "", "path to multi-project eval manifest JSON")
	alphaSweep := flag.Bool("alpha-sweep", false, "sweep alpha values and print table")
	alpha := flag.Float64("alpha", 0.7, "seed vs PPR weight (0=pure PPR, 1=pure seeds)")
	modelName := flag.String("model", "jina-embeddings-v2-base-code", "embedding model name")
	rerankerName := flag.String("reranker", rerank.NoneName, "reranker model name (none, jina-reranker-v1-tiny-en, jina-reranker-v2-base-multilingual, mxbai-rerank-xsmall-v1)")
	rerankK := flag.Int("rerank-k", 15, "candidate count to rerank before returning eval top-10")
	adaptiveRerank := flag.Bool("adaptive-rerank", false, "skip cross-encoder rerank for confident exact/constructor top matches")
	intentRanker := flag.Bool("intent-ranker", false, "apply symbolic query intent ranker after reranking")
	dumpRanking := flag.String("dump-ranking", "", "optional JSONL path for per-query ranking diagnostics")
	holdout := flag.Bool("holdout", false, "use cases_holdout_path instead of cases_path")
	ablation := flag.Bool("ablation", false, "run ablation across rerank/intent/linear configs")
	ftsWeight := flag.Float64("fts-weight", 1.0, "FTS contribution weight in hybrid RRF fusion (1.0 = classic RRF)")
	escalateName := flag.String("escalate-reranker", rerank.NoneName, "stronger reranker applied only to low-confidence queries")
	lazyRerank := flag.Bool("lazy-rerank", false, "run the cross-encoder only when the fused ranking is ambiguous")
	fullBodies := flag.Int("full-bodies", 0, "top results keeping full bodies in packed output (0=default 3, -1=all)")
	flag.Parse()
	retrieve.DefaultFTSWeight = float32(*ftsWeight)
	alphaSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "alpha" {
			alphaSet = true
		}
	})

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	spec, err := embed.GetModel(*modelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "model: %v\n", err)
		return 1
	}

	emb, err := embed.NewOnnxEmbedder(ctx, embed.Config{ModelName: *modelName})
	if err != nil {
		fmt.Fprintf(os.Stderr, "load embedder: %v\n", err)
		return 1
	}
	defer emb.Close()

	rr, closer, err := rerank.New(ctx, rerank.Config{
		ModelName: *rerankerName,
		Log:       log,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "load reranker: %v\n", err)
		return 1
	}
	if closer != nil {
		defer closer.Close()
	}

	escalator, escCloser, err := rerank.New(ctx, rerank.Config{
		ModelName: *escalateName,
		Log:       log,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "load escalation reranker: %v\n", err)
		return 1
	}
	if escCloser != nil {
		defer escCloser.Close()
	}

	var featureRankerInstance retrieve.Ranker
	if *intentRanker {
		featureRankerInstance = intent.NewRanker()
	}

	reqOpts := []func(*retrieve.Request){func(r *retrieve.Request) {
		r.LazyRerank = *lazyRerank
		r.FullBodyResults = *fullBodies
	}}

	if *manifestPath != "" {
		return runManifestEval(ctx, *manifestPath, *alphaSweep, float32(*alpha), alphaSet, *rerankK, *adaptiveRerank, *dumpRanking, *modelName, featureRankerInstance, emb, rr, escalator, log, spec.Dim, *holdout, *ablation, reqOpts)
	}

	return runSingleEval(ctx, *indexPath, *casesPath, *alphaSweep, float32(*alpha), alphaSet, *rerankK, *adaptiveRerank, *dumpRanking, *modelName, featureRankerInstance, emb, rr, escalator, spec.Dim, log, reqOpts)
}

func runSingleEval(
	ctx context.Context,
	indexPath, casesPath string,
	alphaSweep bool,
	alpha float32,
	alphaSet bool,
	rerankK int,
	adaptiveRerank bool,
	dumpRanking, modelName string,
	featureRankerInstance retrieve.Ranker,
	emb retrieve.Embedder,
	rr retrieve.Reranker,
	escalator retrieve.Reranker,
	dim int,
	log *slog.Logger,
	reqOpts []func(*retrieve.Request),
) int {
	st, err := sqlite.New(indexPath, modelName, dim)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open index: %v\n", err)
		return 1
	}
	defer st.Close()

	r := retrieve.NewRetrieverWithRankers(st, emb, rr, featureRankerInstance, log)
	if escalator != nil {
		r.SetEscalator(escalator)
	}
	cases, err := eval.LoadCases(casesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load cases: %v\n", err)
		return 1
	}

	fmt.Printf("Running eval on %d queries...\n\n", len(cases))
	if alphaSweep {
		alphas := []float32{0.0, 0.3, 0.5, 0.7, 1.0}
		fmt.Printf("%-6s | %-9s | %-10s | %s\n", "alpha", "recall@5", "recall@10", "mrr")
		fmt.Printf("%-6s-+-%-9s-+-%-10s-+-%s\n", "------", "---------", "----------", "------")
		for _, a := range alphas {
			results := eval.RunEvalAlphaRerankK(ctx, r, cases, retrieve.ModeHybrid, a, rerankK)
			r5, r10, mrr := eval.Summarize(results)
			fmt.Printf("%-6.1f | %-9.2f | %-10.2f | %.2f\n", a, r5, r10, mrr)
		}
		return 0
	}

	vecResults := eval.RunEvalFullWithAlphaSet(ctx, r, cases, retrieve.ModeVectorOnly, alpha, alphaSet, rerankK, adaptiveRerank, false, false, reqOpts...)
	hybResults := eval.RunEvalFullWithAlphaSet(ctx, r, cases, retrieve.ModeHybrid, alpha, alphaSet, rerankK, adaptiveRerank, false, false, reqOpts...)
	if dumpRanking != "" {
		if err := eval.DumpRankingDiagnostics(dumpRanking, hybResults); err != nil {
			fmt.Fprintf(os.Stderr, "dump ranking diagnostics: %v\n", err)
			return 1
		}
	}
	printEvalReport(vecResults, hybResults)
	return 0
}

func runManifestEval(
	ctx context.Context,
	manifestPath string,
	alphaSweep bool,
	alpha float32,
	alphaSet bool,
	rerankK int,
	adaptiveRerank bool,
	dumpRanking, modelName string,
	featureRankerInstance retrieve.Ranker,
	emb retrieve.Embedder,
	rr retrieve.Reranker,
	escalator retrieve.Reranker,
	log *slog.Logger,
	dim int,
	useHoldout bool,
	ablationMode bool,
	reqOpts []func(*retrieve.Request),
) int {
	if alphaSweep {
		fmt.Fprintln(os.Stderr, "--alpha-sweep is only supported in single-project mode")
		return 1
	}
	manifest, err := eval.LoadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
		return 1
	}

	if ablationMode {
		return runAblation(ctx, manifest, alpha, rerankK, adaptiveRerank, modelName, emb, rr, log, dim, useHoldout)
	}

	projectSummaries := make([]eval.ProjectSummary, 0, len(manifest.Projects))
	var allHybResults []eval.Result
	for _, p := range manifest.Projects {
		casesFilePath := p.CasesPath
		if useHoldout {
			if p.CasesHoldoutPath == "" {
				fmt.Fprintf(os.Stderr, "project %s has no cases_holdout_path\n", p.Name)
				return 1
			}
			casesFilePath = p.CasesHoldoutPath
		}

		st, err := sqlite.New(p.IndexPath, modelName, dim)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open index (%s): %v\n", p.Name, err)
			return 1
		}
		r := retrieve.NewRetrieverWithRankers(st, emb, rr, featureRankerInstance, log)
		if escalator != nil {
			r.SetEscalator(escalator)
		}

		cases, err := eval.LoadCases(casesFilePath)
		if err != nil {
			st.Close()
			fmt.Fprintf(os.Stderr, "load cases (%s): %v\n", p.Name, err)
			return 1
		}

		fmt.Printf("\n=== Project: %s (%d queries) ===\n", p.Name, len(cases))
		vecResults := eval.RunEvalFullWithAlphaSet(ctx, r, cases, retrieve.ModeVectorOnly, alpha, alphaSet, rerankK, adaptiveRerank, false, false, reqOpts...)
		hybResults := eval.RunEvalFullWithAlphaSet(ctx, r, cases, retrieve.ModeHybrid, alpha, alphaSet, rerankK, adaptiveRerank, false, false, reqOpts...)
		allHybResults = append(allHybResults, hybResults...)
		if dumpRanking != "" {
			outPath := dumpPathForProject(dumpRanking, p.Name)
			if err := eval.DumpRankingDiagnostics(outPath, hybResults); err != nil {
				st.Close()
				fmt.Fprintf(os.Stderr, "dump ranking diagnostics (%s): %v\n", p.Name, err)
				return 1
			}
			fmt.Printf("Diagnostics: %s\n", outPath)
		}
		printEvalReport(vecResults, hybResults)
		projectSummaries = append(projectSummaries, eval.BuildProjectSummary(p.Name, vecResults, hybResults))
		st.Close()
	}

	global := eval.SummarizeGlobal(projectSummaries)
	fmt.Printf("\n=== Global Summary (%d queries) ===\n", global.QueryCount)
	printSummaryTable(global.Vector, global.Hybrid)
	printHitCI("Hit@1", global.Hybrid.HitAt1, global.QueryCount)
	printHitCI("Hit@3", global.Hybrid.HitAt3, global.QueryCount)
	printHitCI("R@5", global.Hybrid.RecallAt5, global.QueryCount)
	printHitCI("R@10", global.Hybrid.RecallAt10, global.QueryCount)
	printTagSlices(allHybResults)
	printLatencyBreakdown(allHybResults)
	return 0
}

func printTagSlices(results []eval.Result) {
	summaries, counts := eval.SummarizeByTag(results)
	if len(summaries) == 0 {
		return
	}
	tags := make([]string, 0, len(summaries))
	for t := range summaries {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	fmt.Printf("\n=== Per-Tag Slices (hybrid) ===\n")
	fmt.Printf("%-22s | %-3s | %-5s | %-5s | %-5s | %-5s | %s\n", "tag", "n", "h@1", "h@3", "r@5", "r@10", "mrr")
	fmt.Printf("%-22s-+-%-3s-+-%-5s-+-%-5s-+-%-5s-+-%-5s-+-%s\n", "----------------------", "---", "-----", "-----", "-----", "-----", "-----")
	for _, t := range tags {
		s := summaries[t]
		fmt.Printf("%-22s | %-3d | %-5.2f | %-5.2f | %-5.2f | %-5.2f | %.2f\n",
			t, counts[t], s.HitAt1, s.HitAt3, s.RecallAt5, s.RecallAt10, s.MRR)
	}
}

type ablationConfig struct {
	id           string
	rerankerName string
	skipRerank   bool
	skipIntent   bool
	dppAlpha     float32
	dppSet       bool
	skipDPP      bool
}

func runAblation(
	ctx context.Context,
	manifest eval.Manifest,
	alpha float32,
	rerankK int,
	adaptiveRerank bool,
	modelName string,
	emb retrieve.Embedder,
	_ retrieve.Reranker,
	log *slog.Logger,
	dim int,
	useHoldout bool,
) int {
	configs := []ablationConfig{
		{"baseline", rerank.NoneName, true, true, 0, false, true},
		{"+rerank-jinav2", rerank.JinaRerankerV2BaseMultilingual, false, true, 0, false, true},
		{"+rerank-tiny", rerank.JinaRerankerV1TinyEN, false, true, 0, false, true},
		{"+rerank-xsmall", rerank.MxbaiRerankXsmallV1, false, true, 0, false, true},
		{"+intent", rerank.NoneName, true, false, 0, false, true},
		{"full-jinav2", rerank.JinaRerankerV2BaseMultilingual, false, false, 0, false, true},
		{"full-tiny", rerank.JinaRerankerV1TinyEN, false, false, 0, false, true},
		{"full-xsmall", rerank.MxbaiRerankXsmallV1, false, false, 0, false, true},
	}

	type configRow struct {
		id        string
		hit1      float64
		hit3      float64
		r5        float64
		r10       float64
		ndcg5     float64
		mrr       float64
		rerankP95 time.Duration
		totalP95  time.Duration
		n         int
	}

	var rows []configRow
	var fullResults []eval.Result

	for _, cfg := range configs {
		fmt.Fprintf(os.Stderr, "running ablation config: %s\n", cfg.id)
		rrInst, closer, err := rerank.New(ctx, rerank.Config{
			ModelName: cfg.rerankerName,
			Log:       log,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "SKIP config %s: failed to load reranker %s: %v\n", cfg.id, cfg.rerankerName, err)
			continue
		}

		var allResults []eval.Result
		for _, p := range manifest.Projects {
			casesFilePath := p.CasesPath
			if useHoldout {
				if p.CasesHoldoutPath == "" {
					fmt.Fprintf(os.Stderr, "project %s has no cases_holdout_path\n", p.Name)
					if closer != nil {
						closer.Close()
					}
					return 1
				}
				casesFilePath = p.CasesHoldoutPath
			}

			st, err := sqlite.New(p.IndexPath, modelName, dim)
			if err != nil {
				fmt.Fprintf(os.Stderr, "open index (%s): %v\n", p.Name, err)
				if closer != nil {
					closer.Close()
				}
				return 1
			}

			rankerInstance := retrieve.Ranker(intent.NewRanker())
			r := retrieve.NewRetrieverWithRankers(st, emb, rrInst, rankerInstance, log)

			cases, err := eval.LoadCases(casesFilePath)
			if err != nil {
				st.Close()
				fmt.Fprintf(os.Stderr, "load cases (%s): %v\n", p.Name, err)
				if closer != nil {
					closer.Close()
				}
				return 1
			}

			res := eval.RunEvalFullDPP(ctx, r, cases, retrieve.ModeHybrid, alpha, rerankK, adaptiveRerank,
				cfg.skipRerank, cfg.skipIntent, cfg.dppAlpha, cfg.dppSet, cfg.skipDPP)
			allResults = append(allResults, res...)
			st.Close()
		}

		if closer != nil {
			closer.Close()
		}

		s := eval.SummarizeDetailed(allResults)
		rerankP95 := percentiledDuration(allResults, func(r eval.Result) time.Duration { return r.Stats.RerankDuration }, 95)
		totalP95 := percentiledDuration(allResults, func(r eval.Result) time.Duration { return r.Latency }, 95)
		rows = append(rows, configRow{
			id:        cfg.id,
			hit1:      s.HitAt1,
			hit3:      s.HitAt3,
			r5:        s.RecallAt5,
			r10:       s.RecallAt10,
			ndcg5:     s.NDCGAt5,
			mrr:       s.MRR,
			rerankP95: rerankP95,
			totalP95:  totalP95,
			n:         len(allResults),
		})
		if cfg.id == "full-jinav2" {
			fullResults = allResults
		}
	}

	setLabel := "train"
	if useHoldout {
		setLabel = "holdout"
	}
	fmt.Printf("\n=== Ablation Study (%s set, n per config shown) ===\n", setLabel)
	fmt.Printf("%-16s | %-5s | %-5s | %-5s | %-5s | %-6s | %-5s | %-10s | %-10s | %s\n",
		"config", "h@1", "h@3", "r@5", "r@10", "ndcg5", "mrr", "rerank_p95", "total_p95", "n")
	fmt.Printf("%-16s-+-%-5s-+-%-5s-+-%-5s-+-%-5s-+-%-6s-+-%-5s-+-%-10s-+-%-10s-+-%s\n",
		"----------------", "-----", "-----", "-----", "-----", "------", "-----", "----------", "----------", "---")
	for _, row := range rows {
		rrStr := "-"
		if row.rerankP95 > 0 {
			rrStr = fmtDur(row.rerankP95)
		}
		fmt.Printf("%-16s | %-5.2f | %-5.2f | %-5.2f | %-5.2f | %-6.3f | %-5.2f | %-10s | %-10s | %d\n",
			row.id, row.hit1, row.hit3, row.r5, row.r10, row.ndcg5, row.mrr, rrStr, fmtDur(row.totalP95), row.n)
	}

	if len(rows) > 0 {
		full := rows[len(rows)-1]
		fmt.Printf("\nLast full config CI (%s):\n", full.id)
		printHitCI("Hit@1", full.hit1, full.n)
		printHitCI("Hit@3", full.hit3, full.n)
		printHitCI("R@5", full.r5, full.n)
		printHitCI("R@10", full.r10, full.n)
	}

	printLatencyBreakdown(fullResults)
	return 0
}

func percentiledDuration(results []eval.Result, fn func(eval.Result) time.Duration, pct int) time.Duration {
	vals := make([]time.Duration, 0, len(results))
	for _, r := range results {
		vals = append(vals, fn(r))
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	idx := (len(vals) - 1) * pct / 100
	return vals[idx]
}

func printLatencyBreakdown(results []eval.Result) {
	type stageExtractor struct {
		name string
		fn   func(eval.Result) time.Duration
	}
	stages := []stageExtractor{
		{"embed", func(r eval.Result) time.Duration { return r.Stats.EmbedDuration }},
		{"seed", func(r eval.Result) time.Duration { return r.Stats.SeedDuration }},
		{"graph", func(r eval.Result) time.Duration { return r.Stats.GraphDuration }},
		{"ppr", func(r eval.Result) time.Duration { return r.Stats.PPRDuration }},
		{"rerank", func(r eval.Result) time.Duration { return r.Stats.RerankDuration }},
		{"intent", func(r eval.Result) time.Duration { return r.Stats.IntentDuration }},
		{"escalate", func(r eval.Result) time.Duration { return r.Stats.EscalateDuration }},
		{"pack", func(r eval.Result) time.Duration { return r.Stats.PackDuration }},
		{"total", func(r eval.Result) time.Duration { return r.Latency }},
	}

	var escalated, lazySkipped int
	for _, r := range results {
		if r.Stats.Escalated {
			escalated++
		}
		if r.Stats.RerankLazySkipped {
			lazySkipped++
		}
	}
	if escalated > 0 {
		fmt.Printf("\nEscalated: %d/%d queries (%.0f%%)\n", escalated, len(results), 100*float64(escalated)/float64(len(results)))
	}
	if lazySkipped > 0 {
		fmt.Printf("\nLazy-skipped rerank: %d/%d queries (%.0f%%)\n", lazySkipped, len(results), 100*float64(lazySkipped)/float64(len(results)))
	}

	if len(results) > 0 {
		tokens := make([]int, 0, len(results))
		sum := 0
		for _, r := range results {
			tokens = append(tokens, r.TotalTokens)
			sum += r.TotalTokens
		}
		sort.Ints(tokens)
		fmt.Printf("\nResponse tokens: avg=%d p50=%d p95=%d\n",
			sum/len(results), tokens[(len(tokens)-1)/2], tokens[(len(tokens)-1)*95/100])
	}
	fmt.Printf("\n=== Latency Breakdown (n=%d) ===\n", len(results))
	fmt.Printf("%-8s | %-10s | %-10s | %s\n", "stage", "p50", "p95", "max")
	fmt.Printf("%-8s-+-%-10s-+-%-10s-+-%s\n", "--------", "----------", "----------", "----------")
	for _, s := range stages {
		p50 := percentiledDuration(results, s.fn, 50)
		p95 := percentiledDuration(results, s.fn, 95)
		mx := percentiledDuration(results, s.fn, 100)
		fmt.Printf("%-8s | %-10s | %-10s | %s\n", s.name, fmtDur(p50), fmtDur(p95), fmtDur(mx))
	}
}

func fmtDur(d time.Duration) string {
	if d < time.Millisecond {
		return "<1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// wilsonCI computes 95% Wilson score confidence interval for a proportion.
func wilsonCI(p float64, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	z := 1.96
	nf := float64(n)
	z2n := z * z / nf
	denom := 1 + z2n
	center := (p + z2n/2) / denom
	margin := z * math.Sqrt(p*(1-p)/nf+z2n/(4*nf)) / denom
	return math.Max(0, center-margin), math.Min(1, center+margin)
}

func printHitCI(label string, p float64, n int) {
	lo, hi := wilsonCI(p, n)
	fmt.Printf("%s: %.2f [%.2f, %.2f] (n=%d)\n", label, p, lo, hi, n)
}

func printEvalReport(vecResults, hybResults []eval.Result) {
	vecSummary := eval.SummarizeDetailed(vecResults)
	hybSummary := eval.SummarizeDetailed(hybResults)
	printSummaryTable(vecSummary, hybSummary)

	var totalHybLat time.Duration
	for _, res := range hybResults {
		totalHybLat += res.Latency
	}
	if len(hybResults) > 0 {
		fmt.Printf("\nAvg latency (hybrid): %v\n", totalHybLat/time.Duration(len(hybResults)))
	}

	// Print trap-rate (precision-against-traps) for cases that defined NotExpected.
	var trapCases, hybTrapHits, vecTrapHits int
	for i, hr := range hybResults {
		if i < len(vecResults) {
			vr := vecResults[i]
			if vr.TrapsAt5 > 0 || hr.TrapsAt5 > 0 {
				trapCases++
				vecTrapHits += vr.TrapsAt5
				hybTrapHits += hr.TrapsAt5
			}
		}
	}
	if trapCases > 0 {
		fmt.Printf("\nTrap analysis (cases with not_expected): %d cases\n", trapCases)
		fmt.Printf("  Vector trap hits in top-5: %d\n", vecTrapHits)
		fmt.Printf("  Hybrid trap hits in top-5: %d\n", hybTrapHits)
	}

	fmt.Printf("\n%-30s | %-6s | %-6s | %-6s | %-6s | %-6s | %s\n",
		"Query ID", "V@5", "H@5", "V@10", "H@10", "VMRR", "HMRR")
	fmt.Printf("%-30s-+-%-6s-+-%-6s-+-%-6s-+-%-6s-+-%-6s-+-%s\n",
		"------------------------------", "------", "------", "------", "------", "------", "------")

	vecByID := make(map[string]eval.Result, len(vecResults))
	for _, res := range vecResults {
		vecByID[res.QueryID] = res
	}
	for _, hr := range hybResults {
		vr := vecByID[hr.QueryID]
		vWin := ""
		if vr.RecallAt5 && !hr.RecallAt5 {
			vWin = " <- vec wins"
		}
		hWin := ""
		if hr.RecallAt5 && !vr.RecallAt5 {
			hWin = " <- hyb wins"
		}
		fmt.Printf("%-30s | %-6v | %-6v | %-6v | %-6v | %-6.2f | %.2f%s%s\n",
			hr.QueryID,
			vr.RecallAt5, hr.RecallAt5,
			vr.RecallAt10, hr.RecallAt10,
			vr.MRR, hr.MRR,
			vWin, hWin)
	}
}

func printSummaryTable(vecSummary, hybSummary eval.Summary) {
	fmt.Printf("%-15s | %-11s | %-7s | %s\n", "Metric", "vector-only", "hybrid", "delta")
	fmt.Printf("%-15s-+-%-11s-+-%-7s-+-%s\n", "---------------", "-----------", "-------", "-------")
	fmt.Printf("%-15s | %-11.2f | %-7.2f | %+.2f\n", "Hit@1", vecSummary.HitAt1, hybSummary.HitAt1, hybSummary.HitAt1-vecSummary.HitAt1)
	fmt.Printf("%-15s | %-11.2f | %-7.2f | %+.2f\n", "Hit@3", vecSummary.HitAt3, hybSummary.HitAt3, hybSummary.HitAt3-vecSummary.HitAt3)
	fmt.Printf("%-15s | %-11.2f | %-7.2f | %+.2f\n", "Recall@5", vecSummary.RecallAt5, hybSummary.RecallAt5, hybSummary.RecallAt5-vecSummary.RecallAt5)
	fmt.Printf("%-15s | %-11.2f | %-7.2f | %+.2f\n", "Recall@10", vecSummary.RecallAt10, hybSummary.RecallAt10, hybSummary.RecallAt10-vecSummary.RecallAt10)
	fmt.Printf("%-15s | %-11.3f | %-7.3f | %+.3f\n", "NDCG@5", vecSummary.NDCGAt5, hybSummary.NDCGAt5, hybSummary.NDCGAt5-vecSummary.NDCGAt5)
	fmt.Printf("%-15s | %-11.2f | %-7.2f | %+.2f\n", "MRR", vecSummary.MRR, hybSummary.MRR, hybSummary.MRR-vecSummary.MRR)
}

func dumpPathForProject(basePath, project string) string {
	if filepath.Ext(basePath) == "" {
		return filepath.Join(basePath, sanitizeName(project)+".jsonl")
	}
	ext := filepath.Ext(basePath)
	base := basePath[:len(basePath)-len(ext)]
	return fmt.Sprintf("%s.%s%s", base, sanitizeName(project), ext)
}

var nonAlphaNum = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeName(name string) string {
	clean := nonAlphaNum.ReplaceAllString(name, "_")
	if clean == "" {
		return "project"
	}
	return clean
}
