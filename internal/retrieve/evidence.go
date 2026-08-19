package retrieve

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Evidence-span trimming: large function bodies are the bulk of find_context's
// token cost on big-symbol repos (Prometheus measurement). For each full-body
// result we keep only the window of lines most relevant to the query. Bodies at
// or under evidenceMaxBodyLines are left whole; compact-tier results are
// untouched.
//
// Selection is two-stage. The bi-encoder scores every window (cheap, batched)
// and the cross-encoder re-scores only the shortlist, because it reads query
// and window together and a bag of vectors cannot. Scoring all windows with the
// cross-encoder would cost roughly an order of magnitude more than one rerank
// pass, which is not worth it for a presentational choice.
const (
	evidenceMaxBodyLines = 24
	evidenceWindowLines  = 10
	evidenceWindowStride = 5
	evidenceContextLines = 3
	// evidenceMaxWindows bounds the per-result embedding cost on very long
	// symbols by widening the stride instead of scoring every window.
	evidenceMaxWindows = 64
	// evidenceCEShortlist is how many bi-encoder-ranked windows per result the
	// cross-encoder re-scores. CONTEXTMAXXER_EVIDENCE_CE_TOPK overrides it; 0
	// sends every window (the quality ceiling, for measurement).
	evidenceCEShortlist = 8
)

func ceShortlist() int {
	if v := os.Getenv("CONTEXTMAXXER_EVIDENCE_CE_TOPK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return evidenceCEShortlist
}

type evidenceJob struct {
	ri       int
	winStart []int
	excerpt  string // capped body to restore if windowing cannot finish
}

// applyEvidenceSpans replaces each long full-body result's Body with the most
// query-relevant window (expanded by a little context) and sets BodyStartLine so
// line numbering stays correct. It fails closed onto the capped excerpt: the
// untrimmed body is exactly what the cap exists to prevent.
func applyEvidenceSpans(ctx context.Context, r *Retriever, query string, qvec []float32, results []ScoredResult) {
	for i := range results {
		results[i].BodyStartLine = results[i].StartLine
		results[i].BodyEndLine = results[i].EndLine
	}
	if r.embedder == nil || len(qvec) == 0 {
		return
	}

	jobs, windowTexts := collectWindows(ctx, r, results)
	if len(windowTexts) == 0 {
		return
	}

	vecs, err := r.embedder.Embed(ctx, windowTexts)
	if err != nil || len(vecs) != len(windowTexts) {
		restore(jobs, results)
		return
	}
	scores := make([]float32, len(windowTexts))
	for i := range vecs {
		scores[i] = evidenceDot(vecs[i], qvec)
	}

	best := pickBest(jobs, scores)
	if scorer, ok := r.reranker.(TextScorer); ok && query != "" {
		if refined, ok := refineWithCrossEncoder(ctx, scorer, query, jobs, windowTexts, scores); ok {
			best = refined
		}
	}

	for ji, j := range jobs {
		res := &results[j.ri]
		lines := strings.Split(res.Body, "\n")
		spanStart := best[ji] - evidenceContextLines
		if spanStart < 0 {
			spanStart = 0
		}
		spanEnd := best[ji] + evidenceWindowLines + evidenceContextLines
		if spanEnd > len(lines) {
			spanEnd = len(lines)
		}
		res.Body = strings.Join(lines[spanStart:spanEnd], "\n")
		res.BodyStartLine = res.StartLine + spanStart
		res.BodyEndLine = res.StartLine + spanEnd - 1
		res.Detail = "excerpt"
	}
}

// collectWindows hydrates the lossless body for every windowable result and
// slices it. The indexed body is capped, so windowing that instead could only
// ever surface the head of a long symbol however well it scored — measured on
// prometheus, the shown span held the queried code in 3% of deep-content cases
// (cmd/deepprobe).
func collectWindows(ctx context.Context, r *Retriever, results []ScoredResult) ([]evidenceJob, []string) {
	var jobs []evidenceJob
	var windowTexts []string
	for ri := range results {
		res := &results[ri]
		if res.Detail == "compact" {
			continue
		}
		excerpt := res.Body
		if r.store != nil {
			if full, err := r.store.GetSymbolBody(ctx, res.SymbolID); err == nil && full.Body != "" {
				res.Body = full.Body
			}
		}
		lines := strings.Split(res.Body, "\n")
		if len(lines) <= evidenceMaxBodyLines {
			// Too few lines to window (a long minified line hydrates to one),
			// so the capped excerpt is the only bounded thing to send.
			res.Body = excerpt
			continue
		}
		stride := evidenceWindowStride
		if n := len(lines) / stride; n > evidenceMaxWindows {
			stride = len(lines)/evidenceMaxWindows + 1
		}
		j := evidenceJob{ri: ri, excerpt: excerpt}
		for start := 0; start < len(lines); start += stride {
			end := start + evidenceWindowLines
			if end > len(lines) {
				end = len(lines)
			}
			windowTexts = append(windowTexts, strings.Join(lines[start:end], "\n"))
			j.winStart = append(j.winStart, start)
			if end == len(lines) {
				break
			}
		}
		jobs = append(jobs, j)
	}
	return jobs, windowTexts
}

// pickBest returns, per job, the starting line of its highest-scoring window.
func pickBest(jobs []evidenceJob, scores []float32) []int {
	best := make([]int, len(jobs))
	wi := 0
	for ji, j := range jobs {
		bestScore := float32(-1e30)
		for _, ws := range j.winStart {
			if scores[wi] > bestScore {
				bestScore = scores[wi]
				best[ji] = ws
			}
			wi++
		}
	}
	return best
}

// refineWithCrossEncoder re-scores each job's top bi-encoder windows with the
// cross-encoder. It reports false on any failure so the caller keeps the
// bi-encoder choice rather than losing the trim altogether.
func refineWithCrossEncoder(ctx context.Context, scorer TextScorer, query string, jobs []evidenceJob, windowTexts []string, scores []float32) ([]int, bool) {
	topK := ceShortlist()
	var docs []string
	var docStart []int // window start line for each doc
	var docJob []int   // owning job for each doc
	wi := 0
	for ji, j := range jobs {
		idx := make([]int, len(j.winStart))
		for i := range idx {
			idx[i] = wi + i
		}
		sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
		if topK > 0 && len(idx) > topK {
			idx = idx[:topK]
		}
		for _, gi := range idx {
			docs = append(docs, windowTexts[gi])
			docStart = append(docStart, j.winStart[gi-wi])
			docJob = append(docJob, ji)
		}
		wi += len(j.winStart)
	}
	if len(docs) == 0 {
		return nil, false
	}
	ceScores, err := scorer.ScoreTexts(ctx, query, docs)
	if err != nil || len(ceScores) != len(docs) {
		return nil, false
	}
	best := make([]int, len(jobs))
	bestScore := make([]float32, len(jobs))
	for i := range bestScore {
		bestScore[i] = -1e30
	}
	for i, s := range ceScores {
		if ji := docJob[i]; s > bestScore[ji] {
			bestScore[ji] = s
			best[ji] = docStart[i]
		}
	}
	return best, true
}

func restore(jobs []evidenceJob, results []ScoredResult) {
	for _, j := range jobs {
		results[j.ri].Body = j.excerpt
	}
}

// evidenceDot is the dot product of two embedding vectors. Embeddings are
// L2-normalized by the embedder, so this equals cosine similarity.
func evidenceDot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
