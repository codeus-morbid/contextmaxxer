// Command lateexp is an offline experiment: does late-interaction (ColBERT-style
// MaxSim over per-token vectors) improve paraphrastic SEED recall over the
// production single-vector mean-pool? It scores the WHOLE corpus by both methods
// (not just a re-rank of the seed set) because the hypothesis is that MaxSim
// catches paraphrases the single vector drops at the seed stage — a re-rank of
// an already-missed seed could never show that.
//
// It does NOT change the index format or the hot path; it re-embeds the corpus
// at token level on the fly. Keep it to small/mid indexes (use -max-symbols /
// -sym-tokens to bound memory on large ones).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/eval"
	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/store/sqlite"
)

func main() { os.Exit(run()) }

func run() int {
	indexPath := flag.String("index", ".contextmaxxer/index.db", "path to index db")
	casesPath := flag.String("cases", "internal/eval/testdata/queries.contextmaxxer.gen.json", "eval cases JSON")
	modelName := flag.String("model", "jina-embeddings-v2-base-code", "embedding model")
	category := flag.String("category", "paraphrastic", "keep cases whose Category or a Tag contains this substring; empty = all")
	symTokens := flag.Int("sym-tokens", 256, "cap per-symbol token count (memory/speed bound)")
	maxSymbols := flag.Int("max-symbols", 0, "cap corpus size (0 = all); guards memory on huge indexes")
	flag.Parse()

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	spec, err := embed.GetModel(*modelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "model: %v\n", err)
		return 1
	}

	emb, err := embed.NewOnnxEmbedder(ctx, embed.Config{ModelName: *modelName, Log: log})
	if err != nil {
		fmt.Fprintf(os.Stderr, "load embedder: %v\n", err)
		return 1
	}
	defer emb.Close()

	st, err := sqlite.New(*indexPath, *modelName, spec.Dim)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open index: %v\n", err)
		return 1
	}
	defer st.Close()

	ids, err := st.ListAllSymbolIDs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list symbols: %v\n", err)
		return 1
	}
	if *maxSymbols > 0 && len(ids) > *maxSymbols {
		ids = ids[:*maxSymbols]
	}
	syms, err := st.GetSymbolsByIDs(ctx, ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get symbols: %v\n", err)
		return 1
	}

	fileIDset := map[int64]struct{}{}
	for _, s := range syms {
		fileIDset[s.FileID] = struct{}{}
	}
	fileIDs := make([]int64, 0, len(fileIDset))
	for id := range fileIDset {
		fileIDs = append(fileIDs, id)
	}
	files, err := st.GetFilesByIDs(ctx, fileIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get files: %v\n", err)
		return 1
	}

	// Build the same embedding text the indexer uses, so the mean-pool baseline
	// mirrors what the live index would seed with. (language is approximated
	// from the extension; it is a minor field and identical for both methods.)
	texts := make([]string, len(syms))
	names := make([]string, len(syms))
	inCorpus := map[string]bool{}
	for i, s := range syms {
		path := files[s.FileID]
		texts[i] = index.EmbeddingText(path, langFromPath(path), s)
		names[i] = s.QualifiedName
		inCorpus[s.QualifiedName] = true
	}

	fmt.Fprintf(os.Stderr, "embedding %d symbols at token level...\n", len(syms))
	t0 := time.Now()
	symToks, err := emb.EmbedTokens(ctx, texts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "embed corpus: %v\n", err)
		return 1
	}
	symMean := make([][]float32, len(syms))
	for i := range symToks {
		if len(symToks[i]) > *symTokens {
			symToks[i] = symToks[i][:*symTokens]
		}
		symMean[i] = meanPool(symToks[i], spec.Dim)
	}
	fmt.Fprintf(os.Stderr, "corpus embedded in %s\n", time.Since(t0).Round(time.Millisecond))

	cases, err := eval.LoadCases(*casesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load cases: %v\n", err)
		return 1
	}
	var sel []eval.QueryCase
	for _, c := range cases {
		if *category == "" || matchesCategory(c, *category) {
			sel = append(sel, c)
		}
	}
	if len(sel) == 0 {
		fmt.Fprintf(os.Stderr, "no cases matched category %q\n", *category)
		return 1
	}

	Ks := []int{5, 10, 20, 50}
	var meanHit, maxHit [4]int
	missingTargets := 0

	type flip struct {
		id       string
		meanRank int
		maxRank  int
	}
	var rescued, lost []flip

	for _, c := range sel {
		if !anyInCorpus(c.Expected, inCorpus) {
			missingTargets++
			continue
		}
		qToks, err := emb.EmbedTokens(ctx, []string{c.Query})
		if err != nil {
			fmt.Fprintf(os.Stderr, "embed query %s: %v\n", c.ID, err)
			return 1
		}
		q := qToks[0]
		qMean := meanPool(q, spec.Dim)

		meanScore := make([]float32, len(syms))
		maxScore := make([]float32, len(syms))
		for i := range syms {
			meanScore[i] = dot(qMean, symMean[i])
			maxScore[i] = maxSim(q, symToks[i])
		}
		meanRank := bestRank(names, meanScore, c.Expected)
		maxRank := bestRank(names, maxScore, c.Expected)

		for ki, K := range Ks {
			if meanRank > 0 && meanRank <= K {
				meanHit[ki]++
			}
			if maxRank > 0 && maxRank <= K {
				maxHit[ki]++
			}
		}
		meanIn20 := meanRank > 0 && meanRank <= 20
		maxIn20 := maxRank > 0 && maxRank <= 20
		if maxIn20 && !meanIn20 {
			rescued = append(rescued, flip{c.ID, meanRank, maxRank})
		}
		if meanIn20 && !maxIn20 {
			lost = append(lost, flip{c.ID, meanRank, maxRank})
		}
	}

	n := len(sel) - missingTargets
	fmt.Printf("\n=== Late-interaction vs mean-pool: paraphrastic seed recall ===\n")
	fmt.Printf("index=%s  cases=%s\n", *indexPath, filepath.Base(*casesPath))
	fmt.Printf("corpus=%d symbols  scored cases=%d  (skipped %d: target not in corpus)\n\n",
		len(syms), n, missingTargets)
	if n == 0 {
		fmt.Printf("no scorable cases — check that the index matches the eval corpus\n")
		return 1
	}

	fmt.Printf("%-9s | %-10s | %-10s | %s\n", "recall@", "mean-pool", "maxsim", "delta")
	fmt.Printf("%-9s-+-%-10s-+-%-10s-+-%s\n", "---------", "----------", "----------", "------")
	for ki, K := range Ks {
		mp := float64(meanHit[ki]) / float64(n)
		mx := float64(maxHit[ki]) / float64(n)
		fmt.Printf("@%-8d | %-10.3f | %-10.3f | %+.3f\n", K, mp, mx, mx-mp)
	}

	if len(rescued) > 0 {
		fmt.Printf("\nMaxSim rescued (entered top-20, mean-pool missed it):\n")
		for _, f := range rescued {
			fmt.Printf("  %-28s mean=%s  maxsim=#%d\n", f.id, rankStr(f.meanRank), f.maxRank)
		}
	}
	if len(lost) > 0 {
		fmt.Printf("\nMaxSim regressed (mean-pool had it in top-20, MaxSim dropped it):\n")
		for _, f := range lost {
			fmt.Printf("  %-28s mean=#%d  maxsim=%s\n", f.id, f.meanRank, rankStr(f.maxRank))
		}
	}
	fmt.Printf("\nNet top-20 swing: +%d rescued / -%d regressed\n", len(rescued), len(lost))
	return 0
}

func matchesCategory(c eval.QueryCase, cat string) bool {
	if strings.Contains(c.Category, cat) {
		return true
	}
	for _, t := range c.Tags {
		if strings.Contains(t, cat) {
			return true
		}
	}
	return false
}

func anyInCorpus(expected []string, inCorpus map[string]bool) bool {
	for _, e := range expected {
		if inCorpus[e] {
			return true
		}
	}
	return false
}

// bestRank returns the 1-based rank of the highest-scoring Expected symbol, or 0
// if none of the expected names are in the corpus.
func bestRank(names []string, scores []float32, expected []string) int {
	exp := make(map[string]bool, len(expected))
	for _, e := range expected {
		exp[e] = true
	}
	best := float32(math.Inf(-1))
	found := false
	for i, name := range names {
		if exp[name] && scores[i] > best {
			best = scores[i]
			found = true
		}
	}
	if !found {
		return 0
	}
	rank := 1
	for i, name := range names {
		if !exp[name] && scores[i] > best {
			rank++
		}
	}
	return rank
}

// maxSim is the ColBERT late-interaction score: for each query token, the best
// cosine against any symbol token, summed. Tokens are L2-normalized so dot=cos.
func maxSim(query, sym [][]float32) float32 {
	if len(sym) == 0 {
		return float32(math.Inf(-1))
	}
	var total float32
	for _, qt := range query {
		best := float32(math.Inf(-1))
		for _, st := range sym {
			if d := dot(qt, st); d > best {
				best = d
			}
		}
		total += best
	}
	return total
}

func meanPool(toks [][]float32, dim int) []float32 {
	vec := make([]float32, dim)
	if len(toks) == 0 {
		return vec
	}
	for _, t := range toks {
		for d := 0; d < dim; d++ {
			vec[d] += t[d]
		}
	}
	inv := 1.0 / float32(len(toks))
	for d := range vec {
		vec[d] *= inv
	}
	l2Normalize(vec)
	return vec
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := float32(math.Sqrt(sum))
	if norm < 1e-9 {
		return
	}
	for i := range v {
		v[i] /= norm
	}
}

func rankStr(rank int) string {
	if rank == 0 {
		return "absent"
	}
	return fmt.Sprintf("#%d", rank)
}

func langFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	default:
		return ""
	}
}
