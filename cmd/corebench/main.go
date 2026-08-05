// Command corebench evaluates an embedding model on a downloaded subset of
// CORE-Bench (arXiv:2606.11864, HF: zhangfw123/CORE-Bench) in BEIR format:
// each repo dir holds corpus.jsonl, queries.jsonl and qrels/test.tsv.
//
// It measures the EMBEDDER in our own runtime (tokenizer + pooling + ORT),
// not the full find_context pipeline: corpus chunks are embedded as-is,
// queries are scored against the query's filtered_corpus_id subset (the
// benchmark's temporal filter), and NDCG@10 / Recall@100 are reported per
// repo plus pooled. Corpus embeddings are cached on disk per (repo, model)
// so an A/B between two models only pays each corpus once.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/benchdata"
	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/rerank"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

type (
	corpusDoc = benchdata.CorpusDoc
	query     = benchdata.Query
)

func main() { os.Exit(run()) }

func run() int {
	dataRoot := flag.String("data", "", "root dir; every subdir with corpus.jsonl is a repo")
	modelName := flag.String("model", "jina-embeddings-v2-base-code", "embedding model")
	cacheDir := flag.String("cache", "", "corpus-embedding cache dir (default <data>/embcache)")
	batch := flag.Int("batch", 0, "embed batch size (0 = model default)")
	maxChars := flag.Int("max-chars", 8000, "pre-trim docs/queries to this many bytes before tokenizing")
	mode := flag.String("mode", "embed", "retrieval mode: embed (vector only), hybrid (vector+BM25 RRF), rerank (hybrid + cross-encoder on top-k), graph (hybrid + PPR over a chunk graph)")
	rerankerName := flag.String("reranker", rerank.JinaRerankerV1TinyEN, "cross-encoder for -mode rerank")
	rerankTop := flag.Int("rerank-top", 50, "how many fused candidates the cross-encoder rescores")
	graphAlpha := flag.Float64("graph-alpha", 0.7, "seed weight in the PPR blend for -mode graph (production default)")
	graphSeedK := flag.Int("graph-seeds", 20, "fused candidates used as PPR seeds (production SeedK)")
	flag.Parse()

	if *dataRoot == "" {
		fmt.Fprintln(os.Stderr, "usage: corebench -data <dir> [-model <name>]")
		return 2
	}
	if *cacheDir == "" {
		*cacheDir = filepath.Join(*dataRoot, "embcache")
	}
	if err := os.MkdirAll(*cacheDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "cache dir: %v\n", err)
		return 1
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	emb, err := embed.NewOnnxEmbedder(ctx, embed.Config{ModelName: *modelName, BatchSize: *batch, Log: log})
	if err != nil {
		fmt.Fprintf(os.Stderr, "load embedder: %v\n", err)
		return 1
	}
	defer emb.Close()

	var tsParser index.Parser
	if *mode == "graph" {
		p, err := index.NewTreeSitterParser()
		if err != nil {
			fmt.Fprintf(os.Stderr, "tree-sitter parser: %v\n", err)
			return 1
		}
		defer p.Close()
		tsParser = p
	}

	var rr retrieve.Reranker
	if *mode == "rerank" {
		r, closer, err := rerank.New(ctx, rerank.Config{ModelName: *rerankerName, Log: log})
		if err != nil {
			fmt.Fprintf(os.Stderr, "load reranker: %v\n", err)
			return 1
		}
		if closer != nil {
			defer closer.Close()
		}
		rr = r
	}

	var repos []string
	filepath.WalkDir(*dataRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "corpus.jsonl" {
			repos = append(repos, filepath.Dir(path))
		}
		return nil
	})
	sort.Strings(repos)
	if len(repos) == 0 {
		fmt.Fprintf(os.Stderr, "no corpus.jsonl found under %s\n", *dataRoot)
		return 1
	}

	type repoResult struct {
		name                  string
		queries               int
		ndcg10, recall100     float64
		embedSecs, docsPerSec float64
		queryMs               float64
	}
	var results []repoResult
	var pooledNDCG, pooledRecall float64
	pooledQ := 0

	for _, repoDir := range repos {
		name := repoKey(*dataRoot, repoDir)

		docs, err := benchdata.LoadCorpus(filepath.Join(repoDir, "corpus.jsonl"), *maxChars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: corpus: %v\n", name, err)
			return 1
		}
		queries, err := benchdata.LoadQueries(filepath.Join(repoDir, "queries.jsonl"), *maxChars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: queries: %v\n", name, err)
			return 1
		}
		qrels, err := benchdata.LoadQrels(filepath.Join(repoDir, "qrels", "test.tsv"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: qrels: %v\n", name, err)
			return 1
		}

		idx := make(map[string]int, len(docs))
		for i, d := range docs {
			idx[d.ID] = i
		}

		var bm *benchdata.BM25Index
		if *mode != "embed" {
			bm = benchdata.NewBM25Index(docs)
		}

		var cg *chunkGraph
		if *mode == "graph" {
			cg = buildChunkGraph(tsParser, docs)
			if cg != nil {
				fmt.Fprintf(os.Stderr, "%s: chunk graph lang=%s edges=%d\n", name, cg.lang, cg.edges)
			} else {
				fmt.Fprintf(os.Stderr, "%s: chunk graph: no code language detected, PPR skipped\n", name)
			}
		}

		cachePath := filepath.Join(*cacheDir, benchdata.Sanitize(name)+"."+*modelName+".f32")
		mat, embedSecs, err := corpusMatrix(ctx, emb, docs, cachePath, log)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: embed corpus: %v\n", name, err)
			return 1
		}
		dim := emb.Dimension()

		var sumN, sumR float64
		var qDur time.Duration
		nq := 0
		for _, q := range queries {
			rel := qrels[q.ID]
			if len(rel) == 0 {
				continue
			}
			t0 := time.Now()
			qv, err := emb.EmbedQueries(ctx, []string{q.Text})
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: embed query %s: %v\n", name, q.ID, err)
				return 1
			}
			qDur += time.Since(t0)

			// Temporal filter: rank only within the query's eligible corpus.
			cand := candidateRows(q, idx, len(docs))
			top := make([]scoredRow, 0, len(cand))
			for _, row := range cand {
				top = append(top, scoredRow{row, benchdata.Dot(qv[0], mat[row*dim:(row+1)*dim])})
			}
			sort.Slice(top, func(i, j int) bool { return top[i].s > top[j].s })

			if *mode != "embed" {
				// Mirror the production seeding: RRF-fuse the vector ranking
				// with BM25 over the same candidate set (pipeline.go uses
				// rrfK=60 for its vector+FTS seed fusion).
				top = rrfFuse(top, bm.Rank(q.Text, cand))
			}
			if *mode == "graph" {
				top = applyPPR(cg, top, float32(*graphAlpha), *graphSeedK)
			}
			if rr != nil {
				reranked, err := rerankTopK(ctx, rr, q.Text, top, docs, *rerankTop)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: rerank %s: %v\n", name, q.ID, err)
					return 1
				}
				top = reranked
			}

			rankedIDs := make([]string, len(top))
			for i, sc := range top {
				rankedIDs[i] = docs[sc.row].ID
			}
			sumN += ndcgAt(10, rankedIDs, rel)
			hits := 0
			for i := 0; i < len(rankedIDs) && i < 100; i++ {
				if rel[rankedIDs[i]] > 0 {
					hits++
				}
			}
			sumR += float64(hits) / float64(len(rel))
			nq++
		}
		if nq == 0 {
			continue
		}

		r := repoResult{
			name:    name,
			queries: nq,
			ndcg10:  sumN / float64(nq), recall100: sumR / float64(nq),
			embedSecs: embedSecs, queryMs: float64(qDur.Milliseconds()) / float64(nq),
		}
		if embedSecs > 0 {
			r.docsPerSec = float64(len(docs)) / embedSecs
		}
		results = append(results, r)
		pooledNDCG += sumN
		pooledRecall += sumR
		pooledQ += nq
		fmt.Printf("%-55s q=%-4d NDCG@10=%.4f R@100=%.4f  corpus=%d embed=%.0fs (%.1f docs/s) query=%.0fms\n",
			r.name, r.queries, r.ndcg10, r.recall100, len(docs), r.embedSecs, r.docsPerSec, r.queryMs)
	}

	if pooledQ == 0 {
		fmt.Fprintln(os.Stderr, "no scored queries")
		return 1
	}
	var macroN, macroR float64
	for _, r := range results {
		macroN += r.ndcg10
		macroR += r.recall100
	}
	fmt.Printf("\nmodel=%s mode=%s repos=%d queries=%d\n", *modelName, *mode, len(results), pooledQ)
	fmt.Printf("POOLED  NDCG@10=%.4f Recall@100=%.4f\n", pooledNDCG/float64(pooledQ), pooledRecall/float64(pooledQ))
	fmt.Printf("MACRO   NDCG@10=%.4f Recall@100=%.4f\n", macroN/float64(len(results)), macroR/float64(len(results)))
	return 0
}

type scoredRow struct {
	row int
	s   float32
}

// bm25Index is a small in-memory BM25 (k1=1.2, b=0.75) with the same
// tokenization idea as the store's FTS layer: runs of letters/digits/
// rrfFuse combines the vector ranking with a BM25 row ranking via
// reciprocal-rank fusion, the same scheme (k=60) the production pipeline
// uses for its seed lists.
func rrfFuse(a []scoredRow, bRows []int) []scoredRow {
	const rrfK = 60
	score := map[int]float32{}
	for rank, sc := range a {
		score[sc.row] += 1 / float32(rrfK+rank+1)
	}
	for rank, row := range bRows {
		score[row] += 1 / float32(rrfK+rank+1)
	}
	out := make([]scoredRow, 0, len(score))
	for row, s := range score {
		out = append(out, scoredRow{row, s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].s != out[j].s {
			return out[i].s > out[j].s
		}
		return out[i].row < out[j].row
	})
	return out
}

// rerankTopK rescores the head of the fused ranking with the cross-encoder
// and keeps the tail in fused order (NDCG@10 moves; Recall@100 stays the
// fused ranking's).
func rerankTopK(ctx context.Context, rr retrieve.Reranker, query string, top []scoredRow, docs []corpusDoc, k int) ([]scoredRow, error) {
	if k > len(top) {
		k = len(top)
	}
	cands := make([]retrieve.ScoredResult, k)
	for i := 0; i < k; i++ {
		text := docs[top[i].row].Text
		if len(text) > 2000 {
			text = text[:2000]
		}
		cands[i] = retrieve.ScoredResult{
			QualifiedName: docs[top[i].row].ID, // carries the row identity through
			Body:          text,
			Score:         top[i].s,
		}
	}
	reranked, err := rr.Rerank(ctx, query, cands)
	if err != nil {
		return nil, err
	}
	byID := map[string]int{}
	for i := 0; i < k; i++ {
		byID[docs[top[i].row].ID] = top[i].row
	}
	out := make([]scoredRow, 0, len(top))
	for _, c := range reranked {
		out = append(out, scoredRow{byID[c.QualifiedName], c.Score})
	}
	out = append(out, top[k:]...)
	return out, nil
}

func ndcgAt(k int, rankedIDs []string, rel map[string]int) float64 {
	dcg := 0.0
	for i := 0; i < len(rankedIDs) && i < k; i++ {
		if g := rel[rankedIDs[i]]; g > 0 {
			dcg += (math.Pow(2, float64(g)) - 1) / math.Log2(float64(i)+2)
		}
	}
	gains := make([]int, 0, len(rel))
	for _, g := range rel {
		gains = append(gains, g)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(gains)))
	idcg := 0.0
	for i := 0; i < len(gains) && i < k; i++ {
		idcg += (math.Pow(2, float64(gains[i])) - 1) / math.Log2(float64(i)+2)
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func candidateRows(q query, idx map[string]int, n int) []int {
	if len(q.Filtered) == 0 {
		rows := make([]int, n)
		for i := range rows {
			rows[i] = i
		}
		return rows
	}
	rows := make([]int, 0, len(q.Filtered))
	for _, id := range q.Filtered {
		if row, ok := idx[id]; ok {
			rows = append(rows, row)
		}
	}
	return rows
}

// corpusMatrix returns the L2-normalized doc-embedding matrix (row-major,
// len(docs)*dim), loading it from cachePath when the doc count matches.
func corpusMatrix(ctx context.Context, emb *embed.OnnxEmbedder, docs []corpusDoc, cachePath string, log *slog.Logger) ([]float32, float64, error) {
	dim := emb.Dimension()
	if mat, ok := benchdata.LoadMatrix(cachePath, len(docs), dim); ok {
		return mat, 0, nil
	}
	mat := make([]float32, len(docs)*dim)
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Text
	}
	t0 := time.Now()
	const chunk = 256 // outer chunk: progress logging + bounded peak memory
	for off := 0; off < len(texts); off += chunk {
		end := off + chunk
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := emb.Embed(ctx, texts[off:end])
		if err != nil {
			return nil, 0, err
		}
		for i, v := range vecs {
			copy(mat[(off+i)*dim:], v)
		}
		log.Info("corpus embed progress", "done", end, "total", len(texts))
	}
	secs := time.Since(t0).Seconds()
	if err := benchdata.SaveMatrix(cachePath, mat, len(docs), dim); err != nil {
		log.Warn("save embed cache failed", "err", err)
	}
	return mat, secs, nil
}

func repoKey(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}
