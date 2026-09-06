package retrieve

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// A locator is what an agent is holding after it greps: a file path, usually
// with the line number `rg -n` printed next to it. Until this branch existed
// there was no way to ask this tool about that position at all — find_context
// takes prose, and expand_context takes a request_id from a prior find_context.
// An agent that had already found the place had to throw the position away and
// re-search semantically to get the callers.
//
// Answering a locator costs no embedding, no reranking and no PageRank: the
// file is known, so the work is a lookup in caches the retriever already holds.
type locator struct {
	path string
	// line is the position to centre on; 0 means the whole file was named.
	line int
}

// parseLocator recognises the shapes grep and editors produce:
//
//	src/topics/posts.js:142          rg -n, :line
//	src/topics/posts.js:142:  code   rg -n with the matched line still attached
//	src/topics/posts.js:142-190      an explicit span
//	src/topics/posts.js              the whole file
//
// It only proposes. Nothing is treated as a locator until the path resolves
// against the index (resolveLocator), because a query like "Config: how do we
// load it" splits on a colon just as happily and must stay a semantic search.
func parseLocator(q string) (locator, bool) {
	q = strings.TrimSpace(q)
	if q == "" {
		return locator{}, false
	}
	// A path with the matched source line still attached is the common paste;
	// everything from the third colon-separated field on is that source line.
	parts := strings.SplitN(q, ":", 3)
	path := strings.TrimSpace(parts[0])
	if !looksLikePath(path) {
		return locator{}, false
	}
	loc := locator{path: normalizeLocatorPath(path)}
	if len(parts) == 1 {
		return loc, true
	}
	// The line field may be "142", "142-190", or absent (a trailing colon).
	lineField := strings.TrimSpace(parts[1])
	if lineField == "" {
		return loc, true
	}
	if i := strings.IndexAny(lineField, "-,"); i > 0 {
		lineField = lineField[:i]
	}
	n, err := strconv.Atoi(lineField)
	if err != nil || n <= 0 {
		// "Config: how do we load it" lands here whenever the first word also
		// passed looksLikePath — a non-numeric second field means prose.
		return locator{}, false
	}
	loc.line = n
	return loc, true
}

// LooksLikeLocator reports whether a query is written as a position rather than
// a sentence. Exported so adoption reporting classifies queries by the same rule
// the pipeline routes them with: two copies of this question disagreeing is how
// a measurement ends up describing something the product does not do.
//
// It answers the shape only. Whether the path resolves is a question for the
// index, and the pipeline asks it separately.
func LooksLikeLocator(query string) bool {
	_, ok := parseLocator(query)
	return ok
}

// looksLikePath rejects ordinary words before any index work happens. A path
// has a separator or a short extension, and never a space.
func looksLikePath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return true
	}
	dot := strings.LastIndex(s, ".")
	if dot <= 0 || dot == len(s)-1 {
		return false
	}
	ext := s[dot+1:]
	if len(ext) > 5 {
		return false
	}
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func normalizeLocatorPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

// resolveLocator maps a parsed path onto file IDs in the index.
//
// Agents paste paths relative to wherever they ran grep, which is not always
// the indexed root, so an exact match is tried first and a path-boundary suffix
// match second. Returning no IDs is the signal to fall through to semantic
// search: this is what stops the branch from hijacking a query it cannot serve.
func resolveLocator(loc locator, filePaths map[int64]string) []int64 {
	want := loc.path
	var exact, suffix []int64
	for id, p := range filePaths {
		np := normalizeLocatorPath(p)
		switch {
		case np == want:
			exact = append(exact, id)
		case strings.HasSuffix(np, "/"+want):
			suffix = append(suffix, id)
		}
	}
	out := exact
	if len(out) == 0 {
		out = suffix
	}
	// Sorted so an ambiguous suffix ("posts.js" in four packages) returns the
	// same files in the same order on every run; map iteration would not.
	sort.Slice(out, func(i, j int) bool {
		a, b := filePaths[out[i]], filePaths[out[j]]
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	return out
}

// selectLocatorSymbols picks the symbols a position refers to.
//
// With a line, the innermost enclosing symbol comes first: a method inside a
// class is what the agent pointed at, and the class is context for it. With no
// enclosing symbol — the line is an import block, a bare statement, or a gap —
// the nearest symbols answer instead of nothing, because an agent that pasted a
// real position should never get an empty response.
func selectLocatorSymbols(syms []store.Symbol, fileIDs []int64, line, max int) []store.Symbol {
	inFile := make(map[int64]bool, len(fileIDs))
	for _, id := range fileIDs {
		inFile[id] = true
	}
	var cands []store.Symbol
	for _, s := range syms {
		if inFile[s.FileID] {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return nil
	}

	if line == 0 {
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].FileID != cands[j].FileID {
				return cands[i].FileID < cands[j].FileID
			}
			if cands[i].StartLine != cands[j].StartLine {
				return cands[i].StartLine < cands[j].StartLine
			}
			return cands[i].ID < cands[j].ID
		})
		if len(cands) > max {
			cands = cands[:max]
		}
		return cands
	}

	var enclosing, rest []store.Symbol
	for _, s := range cands {
		if s.StartLine <= line && line <= s.EndLine {
			enclosing = append(enclosing, s)
		} else {
			rest = append(rest, s)
		}
	}
	sort.Slice(enclosing, func(i, j int) bool {
		si := enclosing[i].EndLine - enclosing[i].StartLine
		sj := enclosing[j].EndLine - enclosing[j].StartLine
		if si != sj {
			return si < sj // innermost first
		}
		return enclosing[i].ID < enclosing[j].ID
	})
	dist := func(s store.Symbol) int {
		if line < s.StartLine {
			return s.StartLine - line
		}
		return line - s.EndLine
	}
	sort.Slice(rest, func(i, j int) bool {
		di, dj := dist(rest[i]), dist(rest[j])
		if di != dj {
			return di < dj
		}
		return rest[i].ID < rest[j].ID
	})
	out := append(enclosing, rest...)
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// locatorResults answers a locator query, or reports that it could not.
//
// DECISION(2026-09): a locator answer keeps full bodies rather than
// query-relevant evidence spans. Evidence trimming scores each line against the
// query, and the query here is a file path — trimming a body against it would
// keep whatever lines happen to share tokens with the path. The agent named a
// position; the code at that position is the answer.
// ASSUMES: the packer's full-body cap still bounds the response.
// REVISIT IF: locator responses show up as the heavy ones in cmd/rspbreak.
func locatorResults(ctx context.Context, r *Retriever, req Request) ([]ScoredResult, map[int64]string, bool, error) {
	loc, ok := parseLocator(req.Query)
	if !ok {
		return nil, nil, false, nil
	}
	memo, err := r.getGraphMemo(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	fileIDs := resolveLocator(loc, memo.filePaths)
	if len(fileIDs) == 0 {
		return nil, nil, false, nil
	}
	picked := selectLocatorSymbols(memo.syms, fileIDs, loc.line, req.MaxResults)
	if len(picked) == 0 {
		return nil, nil, false, nil
	}

	// Meta carries no bodies (ListSymbolMeta selects seven columns); hydrate
	// just the handful that were picked.
	ids := make([]int64, len(picked))
	for i, s := range picked {
		ids[i] = s.ID
	}
	full, err := r.store.GetSymbolsByIDs(ctx, ids)
	if err != nil {
		return nil, nil, false, err
	}
	byID := make(map[int64]store.Symbol, len(full))
	for _, s := range full {
		byID[s.ID] = s
	}

	scored := make([]ScoredResult, 0, len(picked))
	for _, meta := range picked {
		s, ok := byID[meta.ID]
		if !ok {
			s = meta
		}
		body := s.BodyExcerpt
		if body == "" {
			body = s.Signature
		}
		// The micro-symbol rule the search branch applies, with one exception:
		// a symbol that encloses the named line is what the agent pointed at
		// and is returned however small it is. The rest are filler around the
		// position and are held to the same bar as any other result.
		encloses := loc.line > 0 && s.StartLine <= loc.line && loc.line <= s.EndLine
		if !req.IncludeTrivial && !encloses {
			if len(body) < 50 && s.EndLine-s.StartLine < 2 {
				continue
			}
		}
		scored = append(scored, ScoredResult{
			SymbolID:      s.ID,
			File:          memo.filePaths[s.FileID],
			QualifiedName: s.QualifiedName,
			Kind:          s.Kind,
			Signature:     s.Signature,
			Docstring:     s.Docstring,
			StartLine:     s.StartLine,
			EndLine:       s.EndLine,
			// Descending synthetic scores: the order is already decided by
			// containment, and a flat score would read downstream as a pile of
			// ties, which is what buildRetrievalHealth calls low confidence.
			Score: 1 - float32(len(scored))/float32(len(picked)+1),
			Why:   "locator",
			Body:  body,
		})
	}
	return scored, memo.filePaths, true, nil
}
