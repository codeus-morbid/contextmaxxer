package retrieve

import "strings"

func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

// defaultFullBodyResults is how many top results keep their full body in the
// packed response; lower-ranked results are compacted to signature+docstring.
//
// DECISION(2026-09): raised 3 -> 6 on a measured sweep over 387 SWE-Explore
// instances at the benchmark's B=500 line budget. Six is not a compromise, it
// DOMINATES three on both axes at once — precision 0.334 -> 0.338 and line
// recall 0.034 -> 0.051 — because a default answer was only 62 lines of code
// against a budget of 500, and the tool was starving the reader rather than
// saving it anything. The real tradeoff starts after six: 12 bodies give
// recall 0.076 at precision 0.308, and all twenty give 0.096 at 0.253.
// ASSUMES: the reader's budget resembles the benchmark's 500 lines.
// REVISIT IF: agents report the response crowding their context — the sweep is
// cmd/exploreprobe -full, so this is one command to re-measure.
const defaultFullBodyResults = 6

// Pack preserves ranking order and selects symbols until budget is exhausted.
// DECISION(2026-06): tiered packing — only the top fullBodyCount results carry
// the full body; the tail is compacted to signature + first docstring line
// (~10-20x cheaper per result). The consumer goal is information per token:
// a correct answer at rank 5 should cost the agent a glance, not a screenful.
// fullBodyCount < 0 disables tiering (every result keeps its body).
func Pack(symbols []ScoredResult, budgetTokens int, fullBodyCount int) ([]ScoredResult, int) {
	if len(symbols) == 0 {
		return nil, 0
	}
	if fullBodyCount == 0 {
		fullBodyCount = defaultFullBodyResults
	}

	var selected []ScoredResult
	total := 0
	for i, s := range symbols {
		if fullBodyCount >= 0 && i >= fullBodyCount {
			s.Body = compactBody(s)
			s.Detail = "compact"
			// DECISION(2026-09): a compacted result reports the lines it ACTUALLY
			// shows. Without this its span stayed the symbol's full extent, so a
			// two-line signature was announced as `lines: 100-180` — the reader,
			// agent or benchmark, was told it had seen eighty lines it never got.
			// Measured cost of the old behaviour on SWE-Explore: seventeen of
			// twenty results claimed their whole symbol, which inflated line
			// recall, deflated precision, and burned a 500-line budget on text
			// nobody was sent.
			s.BodyStartLine = s.StartLine
			s.BodyEndLine = s.StartLine + strings.Count(s.Body, "\n")
			s.BodySegments = nil
		} else {
			s.Detail = "full"
		}
		cost := estimateTokens(s.Body)
		if cost == 0 {
			cost = 1
		}
		if total+cost > budgetTokens {
			break
		}
		selected = append(selected, s)
		total += cost
	}
	return selected, total
}

// compactBody renders the signature plus the first docstring line — enough
// for an agent to decide whether to open the symbol, at a fraction of the
// body cost.
func compactBody(s ScoredResult) string {
	sig := s.Signature
	if sig == "" {
		if idx := strings.IndexByte(s.Body, '\n'); idx > 0 {
			sig = s.Body[:idx]
		} else {
			sig = s.Body
		}
	}
	doc := s.Docstring
	if idx := strings.IndexByte(doc, '\n'); idx >= 0 {
		doc = doc[:idx]
	}
	if doc == "" {
		return sig
	}
	return sig + "\n// " + doc
}
