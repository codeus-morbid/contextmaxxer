package retrieve

import (
	"context"
	"strings"
)

// Evidence-span trimming: large function bodies are the bulk of find_context's
// token cost on big-symbol repos (Prometheus measurement). For each full-body
// result we keep only the window of lines most relevant to the query — scored
// by embedding cosine against the query vector, which is query-specific and so
// works on paraphrastic queries (unlike keyword highlighting). Bodies at or
// under evidenceMaxBodyLines are left whole; compact-tier results are untouched.
const (
	evidenceMaxBodyLines = 24
	evidenceWindowLines  = 10
	evidenceWindowStride = 5
	evidenceContextLines = 3
	// evidenceMaxWindows bounds the per-result embedding cost on very long
	// symbols by widening the stride instead of scoring every window.
	evidenceMaxWindows = 64
)

// applyEvidenceSpans replaces each long full-body result's Body with the most
// query-relevant window (expanded by a little context) and sets BodyStartLine so
// line numbering stays correct. It fails open (keeps the full body) on any error.
func applyEvidenceSpans(ctx context.Context, r *Retriever, qvec []float32, results []ScoredResult) {
	for i := range results {
		results[i].BodyStartLine = results[i].StartLine
		results[i].BodyEndLine = results[i].EndLine
	}
	if r.embedder == nil || len(qvec) == 0 {
		return
	}

	var windowTexts []string
	type job struct {
		ri       int
		winStart []int
		excerpt  string // capped body to restore if windowing cannot finish
	}
	var jobs []job

	for ri := range results {
		res := &results[ri]
		if res.Detail == "compact" {
			continue
		}
		// The indexed body is capped, so windowing it can only ever surface the
		// head of a long symbol — measured on prometheus, the shown span
		// contained the queried code in 3% of deep-content cases (cmd/deepprobe).
		// Hydrate the lossless body first so the window is chosen over the whole
		// function; the excerpt is restored if the scoring pass cannot run.
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
		j := job{ri: ri, excerpt: excerpt}
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
	if len(windowTexts) == 0 {
		return
	}

	vecs, err := r.embedder.Embed(ctx, windowTexts)
	if err != nil || len(vecs) != len(windowTexts) {
		// Never leave a hydrated body in place: without scoring there is no
		// window to trim it to, and the untrimmed body is what the cap existed
		// to avoid sending.
		for _, j := range jobs {
			results[j.ri].Body = j.excerpt
		}
		return
	}

	wi := 0
	for _, j := range jobs {
		res := &results[j.ri]
		lines := strings.Split(res.Body, "\n")
		bestScore := float32(-1e30)
		bestStart := 0
		for _, ws := range j.winStart {
			if s := evidenceDot(vecs[wi], qvec); s > bestScore {
				bestScore = s
				bestStart = ws
			}
			wi++
		}
		spanStart := bestStart - evidenceContextLines
		if spanStart < 0 {
			spanStart = 0
		}
		spanEnd := bestStart + evidenceWindowLines + evidenceContextLines
		if spanEnd > len(lines) {
			spanEnd = len(lines)
		}
		res.Body = strings.Join(lines[spanStart:spanEnd], "\n")
		res.BodyStartLine = res.StartLine + spanStart
		res.BodyEndLine = res.StartLine + spanEnd - 1
		res.Detail = "excerpt"
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
