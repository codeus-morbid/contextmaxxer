package retrieve

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// The literal channel: file-level evidence from exact identifier matches.
//
// Everything else in this pipeline scores symbols independently. This one asks a
// different question — how many of the names the question uses occur in the same
// FILE, each weighted by how rare it is in this repository.
//
// DECISION(2026-09): shipped OFF, because the idea that motivated it did not
// survive its own measurement.
//
// An offline simulation was promising: over 150 SWE-Explore instances, swapping
// the tail of an 18-file answer for the top candidates of a filesystem grep
// moved file recall 0.596 -> 0.660, better on 34 instances and worse on 5. Under
// the benchmark's actual protocol the channel returns none of that. On the nine
// snapshots whose corpus genuinely contains the extra test files, adding it to
// that corpus gives file recall 0.643 -> 0.615: better on ZERO instances, worse
// on one. It fires as designed — a debug response shows exactly the requested
// literal_match results, in real files — so this is the idea failing, not the
// wiring.
//
// The gap between the two is the corpus, and the corpus is where the value
// turned out to be: the same nine snapshots go 0.517 -> 0.643 file recall from
// indexing name-conventioned tests alone, with precision rising rather than
// falling. What the simulation credited to literal matching was mostly the
// files it was allowed to see.
//
// ASSUMES: the simulation's advantage came from grepping whole file text, where
// this channel matches the indexed head and the capped body, so it sees less of
// each file than grep does.
// REVISIT IF: bodies stop being capped (see the ~5% of symbols with 53% of their
// code invisible), which is the one difference that could still explain the gap.

// literalIdentifier matches the shapes an issue uses to name code. Kept in step
// with cmd/exploreprobe/query.go, which measured these same patterns.
var (
	reLitBackticked = regexp.MustCompile("`([^`\n]{2,80})`")
	reLitCamel      = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z0-9]*\b|\b[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]*\b`)
	reLitSnake      = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
)

func literalStopWord(s string) bool {
	switch strings.ToLower(s) {
	case "e.g", "i.e", "etc", "vs", "self", "true", "false", "none", "null",
		"the", "and", "for", "with", "this", "that", "when", "then", "should",
		"github.com", "readme.md", "python", "javascript":
		return true
	}
	return false
}

// queryIdentifiers pulls the names a question uses for code, most distinctive
// first — backticked spans are the author's own emphasis, so they lead.
//
// Only single FTS tokens are kept. A path like "src/topics/posts.js" is
// tokenized by the store into src OR topics OR posts OR js, which matches
// hundreds of files and would destroy the exactness this channel exists for.
// Positions are the locator branch's job, not this one's.
func queryIdentifiers(text string, limit int) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.Trim(s, "`.,;:()[]{}\"'")
		if len(s) < 4 || len(s) > 60 || seen[strings.ToLower(s)] || literalStopWord(s) {
			return
		}
		if !singleFTSToken(s) {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	for _, m := range reLitBackticked.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, re := range []*regexp.Regexp{reLitSnake, reLitCamel} {
		for _, m := range re.FindAllString(text, -1) {
			add(m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// singleFTSToken reports whether the store's tokenizer would keep this as one
// term: letters, digits and underscore only.
func singleFTSToken(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return s != ""
}

// literalCandidate is one file's evidence, reduced to the best symbol in it.
type literalCandidate struct {
	symbol  store.Symbol
	file    string
	score   float32
	matched int
}

const (
	// literalPerTerm caps how many symbols one identifier contributes. It also
	// bounds the rarity estimate: a term that fills the cap is common by
	// definition, which is exactly what the weighting needs to know.
	literalPerTerm = 200
	// literalMinMatches is how many distinct identifiers must occur in a file.
	// One match returns 192 files per query and buries the answer; two returns
	// 57 and carries the whole measured gain; three costs recall for less noise.
	literalMinMatches = 2
	literalMaxTerms   = 8
)

// literalCandidates ranks files by how many of the query's identifiers occur in
// them, weighting each identifier by how rare it is in this repository. A name
// in four hundred files says nothing; a name in three says where to look.
func literalCandidates(ctx context.Context, r *Retriever, query string) ([]literalCandidate, error) {
	ids := queryIdentifiers(query, literalMaxTerms)
	if len(ids) < literalMinMatches {
		return nil, nil
	}
	// The memo's map covers every indexed file. The pipeline's own filePaths
	// holds only the candidate files, and a literal candidate is by definition
	// one the candidate set did not reach — it would come back with no path.
	memo, err := r.getGraphMemo(ctx)
	if err != nil {
		return nil, err
	}
	filePaths := memo.filePaths

	type fileEvidence struct {
		terms map[int]bool
		best  store.Symbol
		bestS float32
	}
	byFile := make(map[int64]*fileEvidence)
	df := make([]int, len(ids))

	for i, id := range ids {
		seenFiles := make(map[int64]bool)
		head, err := r.store.SearchByText(ctx, id, literalPerTerm)
		if err != nil {
			return nil, err
		}
		body, err := r.store.SearchByBodyText(ctx, id, literalPerTerm)
		if err != nil {
			return nil, err
		}
		for _, hits := range [][]store.ScoredSymbol{head, body} {
			for _, h := range hits {
				fe := byFile[h.FileID]
				if fe == nil {
					fe = &fileEvidence{terms: make(map[int]bool)}
					byFile[h.FileID] = fe
				}
				fe.terms[i] = true
				if h.Score > fe.bestS || fe.best.ID == 0 {
					fe.best, fe.bestS = h.Symbol, h.Score
				}
				seenFiles[h.FileID] = true
			}
		}
		df[i] = len(seenFiles)
	}

	out := make([]literalCandidate, 0, len(byFile))
	for fileID, fe := range byFile {
		if len(fe.terms) < literalMinMatches {
			continue
		}
		var s float64
		for i := range fe.terms {
			s += 1.0 / math.Log(2+float64(df[i]))
		}
		out = append(out, literalCandidate{
			symbol:  fe.best,
			file:    filePaths[fileID],
			score:   float32(s),
			matched: len(fe.terms),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// Deterministic: these were collected by ranging a map.
		return out[i].symbol.ID < out[j].symbol.ID
	})
	return out, nil
}

// reserveLiteralSlots hands the last `slots` places of the answer to literal
// candidates the ranking did not already reach.
//
// It replaces the tail rather than extending the answer because that is what
// was measured: at ten results the swap is a wash (0.565 vs 0.558) and at
// eighteen it is worth +0.064, which says the tail past about rank ten has
// stopped discriminating and is the cheap thing to spend.
func reserveLiteralSlots(scored []ScoredResult, cands []literalCandidate, maxResults, slots int) []ScoredResult {
	if slots <= 0 || maxResults <= 0 || len(cands) == 0 {
		return scored
	}
	if slots >= maxResults {
		slots = maxResults - 1
	}
	if slots <= 0 {
		return scored
	}
	keep := maxResults - slots
	if keep > len(scored) {
		keep = len(scored)
	}
	head := append([]ScoredResult(nil), scored[:keep]...)

	inHead := make(map[int64]bool, len(head))
	inFile := make(map[string]bool, len(head))
	for _, s := range head {
		inHead[s.SymbolID] = true
		inFile[s.File] = true
	}

	// The tail keeps its own order behind whatever is promoted, so nothing is
	// lost outright — a promoted candidate pushes the tail down, and the packer
	// still cuts by budget.
	var promoted []ScoredResult
	for _, c := range cands {
		if len(promoted) >= slots {
			break
		}
		if inHead[c.symbol.ID] || inFile[c.file] {
			continue
		}
		body := c.symbol.BodyExcerpt
		if body == "" {
			body = c.symbol.Signature
		}
		inFile[c.file] = true
		promoted = append(promoted, ScoredResult{
			SymbolID:      c.symbol.ID,
			File:          c.file,
			QualifiedName: c.symbol.QualifiedName,
			Kind:          c.symbol.Kind,
			Signature:     c.symbol.Signature,
			Docstring:     c.symbol.Docstring,
			StartLine:     c.symbol.StartLine,
			EndLine:       c.symbol.EndLine,
			// Below the head it replaces, so it never outranks a searched
			// result: this channel is for the slots the ranking has given up on.
			Score: 0,
			Why:   "literal_match",
			Body:  body,
		})
	}
	if len(promoted) == 0 {
		return scored
	}

	out := append(head, promoted...)
	for _, s := range scored[keep:] {
		if inHead[s.SymbolID] {
			continue
		}
		out = append(out, s)
	}
	return out
}
