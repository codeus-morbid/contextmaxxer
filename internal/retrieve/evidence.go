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
	// evidenceWindowsPerResult is how many of the ranked windows are shown.
	// Measured: one window held the queried code 45% of the time on prometheus
	// and 56% on cockroach, so the answer was often in the runner-up. Overriding
	// via CONTEXTMAXXER_EVIDENCE_WINDOWS.
	evidenceWindowsPerResult = 3
	// evidenceKeepRatio is how close to the best a runner-up window must score
	// to be shown alongside it.
	evidenceKeepRatio = 0.6
)

// EvidenceGapMarker separates non-adjacent windows in a trimmed body.
const EvidenceGapMarker = "// ..."

func windowsPerResult() int {
	if v := os.Getenv("CONTEXTMAXXER_EVIDENCE_WINDOWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return evidenceWindowsPerResult
}

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

	ranked := rankWindows(jobs, scores)
	if scorer, ok := r.reranker.(TextScorer); ok && query != "" {
		if refined, ok := refineWithCrossEncoder(ctx, scorer, query, jobs, windowTexts, scores); ok {
			ranked = refined
		}
	}

	// DECISION(2026-08): windows scale with rank. On agent-style questions code
	// is 46% of the response (cmd/rspbreak) — bodies, not boilerplate, are the
	// payload — and the runner-up bodies are alternatives the agent reads only
	// if the first one was wrong. The top result keeps the full budget because
	// that is where the answer usually is; below it one window says "this is
	// what this candidate is about" for a third of the tokens. ASSUMES: an
	// answer ranked 2nd or 3rd is worth one window plus an expand, not three.
	// REVISIT IF: deepprobe coverage below rank 1 turns out to carry the result.
	top := windowsPerResult()
	for ji, j := range jobs {
		res := &results[j.ri]
		keep := 1
		if j.ri == 0 {
			keep = top
		}
		lines := strings.Split(res.Body, "\n")
		spans := mergeSpans(topSpans(ranked[ji], keep, len(lines)))
		renderSegments(res, lines, spans)
	}
}

// span is a half-open line range within the body.
type span struct{ start, end int }

// topSpans expands the best-ranked windows into context-padded ranges. Beyond
// the first it keeps only windows that scored comparably: when the scorer picked
// one window decisively there is nothing to add, and showing runners-up anyway
// would spend tokens on code it just ruled out.
func topSpans(ranked []rankedWindow, keep, bodyLines int) []span {
	if keep > len(ranked) {
		keep = len(ranked)
	}
	out := make([]span, 0, keep)
	for i, w := range ranked[:keep] {
		if i > 0 && !comparableScore(w.score, ranked[0].score) {
			break
		}
		lo := w.start - evidenceContextLines
		if lo < 0 {
			lo = 0
		}
		hi := w.start + evidenceWindowLines + evidenceContextLines
		if hi > bodyLines {
			hi = bodyLines
		}
		out = append(out, span{lo, hi})
	}
	return out
}

// comparableScore reports whether a runner-up is close enough to the best to be
// worth showing. Both scorers produce higher-is-better scores on a bounded
// scale, so a ratio works; a non-positive top score carries no information and
// every window is then equally (un)justified.
func comparableScore(score, top float32) bool {
	if top <= 0 {
		return true
	}
	return score >= top*evidenceKeepRatio
}

// mergeSpans sorts the ranges and folds overlapping or touching ones together,
// so two adjacent winning windows read as one block instead of a block, a gap
// marker, and the very next line.
func mergeSpans(spans []span) []span {
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := []span{spans[0]}
	for _, s := range spans[1:] {
		last := &out[len(out)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// renderSegments writes the chosen ranges into the result, joined by a gap
// marker. BodySegments carries each range's real first line, because numbering
// straight through the marker would misattribute every line after a gap.
func renderSegments(res *ScoredResult, lines []string, spans []span) {
	if len(spans) == 0 {
		return
	}
	parts := make([]string, 0, len(spans))
	segs := make([]BodySegment, 0, len(spans))
	for _, s := range spans {
		parts = append(parts, strings.Join(lines[s.start:s.end], "\n"))
		segs = append(segs, BodySegment{StartLine: res.StartLine + s.start, Lines: s.end - s.start})
	}
	res.Body = strings.Join(parts, "\n"+EvidenceGapMarker+"\n")
	res.BodyStartLine = segs[0].StartLine
	res.BodyEndLine = res.StartLine + spans[len(spans)-1].end - 1
	res.Detail = "excerpt"
	if len(segs) > 1 {
		res.BodySegments = segs
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

// rankedWindow is a candidate window with the score that ranked it.
type rankedWindow struct {
	start int
	score float32
}

// rankWindows returns, per job, its windows ordered best score first.
func rankWindows(jobs []evidenceJob, scores []float32) [][]rankedWindow {
	ranked := make([][]rankedWindow, len(jobs))
	wi := 0
	for ji, j := range jobs {
		out := make([]rankedWindow, len(j.winStart))
		for i, ws := range j.winStart {
			out[i] = rankedWindow{start: ws, score: scores[wi+i]}
		}
		sort.SliceStable(out, func(a, b int) bool { return out[a].score > out[b].score })
		ranked[ji] = out
		wi += len(j.winStart)
	}
	return ranked
}

// refineWithCrossEncoder re-scores each job's top bi-encoder windows with the
// cross-encoder. It reports false on any failure so the caller keeps the
// bi-encoder choice rather than losing the trim altogether.
func refineWithCrossEncoder(ctx context.Context, scorer TextScorer, query string, jobs []evidenceJob, windowTexts []string, scores []float32) ([][]rankedWindow, bool) {
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
	perJob := make([][]rankedWindow, len(jobs))
	for i := range docs {
		ji := docJob[i]
		perJob[ji] = append(perJob[ji], rankedWindow{start: docStart[i], score: ceScores[i]})
	}
	for ji := range perJob {
		w := perJob[ji]
		sort.SliceStable(w, func(a, b int) bool { return w[a].score > w[b].score })
	}
	return perJob, true
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
