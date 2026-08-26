// Command deepprobe measures what ranking metrics structurally cannot see:
// whether the answer is inside the excerpt the response actually shows.
//
// Every retrieval channel (vector, FTS, rerank) reads body_excerpt, which the
// extractor caps, and evidence-span trimming picks its window from that same
// capped text. So for a long symbol the response can only ever show the head.
// Our self-retrieval corpus cannot detect this: it queries a symbol by its
// docstring, and docstrings sit in the head.
//
// A case here is (symbol, probe line, query), where the probe line is real code
// from the part of the body that the excerpt drops. Two rates are reported and
// must not be conflated:
//
//	retrieved — the owning symbol reached top-k at all (a recall property of
//	            the vector/FTS channels)
//	covered   — the shown span contains the probe line (a presentation
//	            property of evidence-span selection)
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	_ "modernc.org/sqlite"
)

const truncationMarker = "// ... [truncated]"

// fullBodyTier mirrors the server's FullBodyResults: below it, packing replaces
// the body with a signature line, so window selection never runs.
var fullBodyTier = 3

type probeCase struct {
	Qualified  string
	Path       string
	StartLine  int
	ExcerptEnd int // last file line the capped excerpt can possibly show
	ProbeLine  int // absolute file line of the probe
	Depth      int // probe offset past the last excerpt line
	Tokens     []string
}

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db to probe")
	n := flag.Int("n", 60, "number of truncated symbols to sample (stride over all of them)")
	maxResults := flag.Int("max", 10, "max_results per call")
	mode := flag.String("mode", "both", "presentation | recall | both")
	verbose := flag.Bool("v", false, "print per-case outcomes")
	tier := flag.Int("full-tier", fullBodyTier, "ranks that keep a full (windowed) body server-side")
	flag.Parse()
	fullBodyTier = *tier

	cases, total, err := loadCases(*indexPath, *n)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load cases:", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "no probeable truncated symbols in this index (v1 format, or nothing long enough)")
		os.Exit(1)
	}

	srv, err := evalharness.Start(*bin, *indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start server:", err)
		os.Exit(1)
	}
	defer srv.Stop()

	fmt.Printf("index=%s truncated=%d probed=%d avg_depth=%d lines past the excerpt\n",
		*indexPath, total, len(cases), avgDepth(cases))

	if *mode == "presentation" || *mode == "both" {
		run(srv, cases, *maxResults, *verbose, true)
	}
	if *mode == "recall" || *mode == "both" {
		run(srv, cases, *maxResults, *verbose, false)
	}
}

// run reports one mode. withName decides whether the query may lean on the
// symbol's own name: with it, ranking is nearly free and the covered rate
// isolates evidence-span selection; without it, the query is only about deep
// content and the retrieved rate isolates the seed channels.
func run(srv *evalharness.Server, cases []probeCase, maxResults int, verbose, withName bool) {
	label := "recall (deep content only)"
	if withName {
		label = "presentation (name + deep content)"
	}
	var retrieved, covered, inTier, coveredInTier, shownLines int
	rankHist := map[int]int{}
	for _, c := range cases {
		q := strings.Join(c.Tokens, " ")
		if withName {
			q = identifierWords(c.Qualified) + " " + q
		}
		res, err := srv.Find(q, maxResults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.Qualified, err)
			os.Exit(1)
		}
		rank := -1
		for i, name := range res.Names {
			if name == c.Qualified {
				rank = i
				break
			}
		}
		if rank < 0 {
			if verbose {
				fmt.Printf("  MISS  %-58s q=%q\n", c.Qualified, truncate(q, 60))
			}
			continue
		}
		retrieved++
		spans := parseSpans(visibleSpan(res, rank, c))
		hit := coversLine(spans, c.ProbeLine)
		lo, hi := 0, 0
		if len(spans) > 0 {
			lo, hi = spans[0].lo, spans[len(spans)-1].hi
		}
		if hit {
			covered++
		}
		// Only the top tier carries a windowed body at all; below it results
		// pack down to a signature line, so a miss there is a packing decision
		// and not a failure of window selection.
		if rank < fullBodyTier {
			rankHist[rank]++
			inTier++
			for _, s := range spans {
				shownLines += s.hi - s.lo + 1
			}
			if hit {
				coveredInTier++
			}
		}
		if verbose && !hit {
			fmt.Printf("  BLIND %-58s probe=%d shown=%d-%d (+%d past excerpt)\n",
				c.Qualified, c.ProbeLine, lo, hi, c.Depth)
		}
	}
	nn := len(cases)
	fmt.Printf("%-34s retrieved=%.2f (%d/%d)", label, rate(retrieved, nn), retrieved, nn)
	if withName {
		fmt.Printf("  covered_in_tier=%.2f (%d/%d)  covered_of_retrieved=%.2f (%d/%d)  avg_lines_shown=%.1f",
			rate(coveredInTier, inTier), coveredInTier, inTier,
			rate(covered, retrieved), covered, retrieved,
			rate(shownLines, inTier))
		// Which ranks the probe actually exercises: a metric that only ever
		// lands on rank 1 cannot price a change made to ranks 2 and 3.
		fmt.Printf("  ranks:")
		for r := 0; r < fullBodyTier; r++ {
			fmt.Printf(" %d=%d", r+1, rankHist[r])
		}
	}
	fmt.Println()
}

// visibleSpan is what the agent can actually read. When the response reports no
// trimmed span, the body it carries is still the capped excerpt, NOT the whole
// symbol — res.Lines names the symbol's full extent and would silently count
// every capped body as covering its own tail.
func visibleSpan(res evalharness.Result, rank int, c probeCase) string {
	if rank < len(res.Visible) && res.Visible[rank] != "" {
		return res.Visible[rank]
	}
	return fmt.Sprintf("%d-%d", c.StartLine, c.ExcerptEnd)
}

type lineSpan struct{ lo, hi int }

// parseSpans reads the visible-lines field, which names one range per shown
// window ("701-716,760-775"). Reading it as a single first-to-last range would
// count the gaps between windows as visible.
func parseSpans(s string) []lineSpan {
	var out []lineSpan
	for _, part := range strings.Split(s, ",") {
		ends := strings.SplitN(strings.TrimSpace(part), "-", 2)
		if len(ends) != 2 {
			continue
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(ends[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(ends[1]))
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, lineSpan{lo, hi})
	}
	return out
}

func coversLine(spans []lineSpan, line int) bool {
	for _, s := range spans {
		if line >= s.lo && line <= s.hi {
			return true
		}
	}
	return false
}

// loadCases samples truncated symbols and picks, for each, the most
// identifier-dense line in the part of the body the excerpt drops.
func loadCases(path string, want int) ([]probeCase, int, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT s.qualified_name, s.start_line, s.body_excerpt, b.body, f.path
		FROM symbol_bodies b
		JOIN symbols s ON s.id = b.symbol_id
		JOIN files f ON f.id = s.file_id
		WHERE s.kind IN ('function','method')
		ORDER BY s.id`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var all []probeCase
	total := 0
	for rows.Next() {
		var qname, excerpt, body, fpath string
		var startLine int
		if err := rows.Scan(&qname, &startLine, &excerpt, &body, &fpath); err != nil {
			return nil, 0, err
		}
		total++
		c, ok := buildCase(qname, fpath, startLine, excerpt, body)
		if ok {
			all = append(all, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return stride(all, want), total, nil
}

func buildCase(qname, fpath string, startLine int, excerpt, body string) (probeCase, bool) {
	visible := strings.Count(strings.TrimSuffix(excerpt, "\n"+truncationMarker), "\n") + 1
	lines := strings.Split(body, "\n")
	if len(lines) <= visible+5 {
		return probeCase{}, false
	}
	bestIdx, bestTokens := -1, []string(nil)
	for i := visible; i < len(lines); i++ {
		toks := codeTokens(lines[i])
		if len(toks) > len(bestTokens) {
			bestIdx, bestTokens = i, toks
		}
	}
	if bestIdx < 0 || len(bestTokens) < 3 {
		return probeCase{}, false
	}
	return probeCase{
		Qualified:  qname,
		Path:       fpath,
		StartLine:  startLine,
		ExcerptEnd: startLine + visible - 1,
		ProbeLine:  startLine + bestIdx,
		Depth:      bestIdx - visible,
		Tokens:     bestTokens,
	}, true
}

// codeTokens returns the distinct identifier words on a line of code, or nil
// for lines that carry no searchable content (comments, punctuation, closers).
func codeTokens(line string) []string {
	t := strings.TrimSpace(line)
	if len(t) < 12 || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") ||
		strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "#") {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.FieldsFunc(splitCamel(t), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		w = strings.ToLower(w)
		if len(w) < 4 || goKeywords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

var goKeywords = map[string]bool{
	"func": true, "return": true, "range": true, "case": true, "else": true,
	"true": true, "false": true, "break": true, "continue": true, "default": true,
	"nil": true, "type": true, "struct": true, "interface": true, "import": true,
	"switch": true, "select": true, "defer": true, "string": true, "error": true,
}

func splitCamel(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			b.WriteRune(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func identifierWords(qname string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, splitCamel(qname))), " ")
}

func stride(all []probeCase, want int) []probeCase {
	if want <= 0 || len(all) <= want {
		return all
	}
	step := float64(len(all)) / float64(want)
	out := make([]probeCase, 0, want)
	for i := 0; i < want; i++ {
		out = append(out, all[int(float64(i)*step)])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Qualified < out[j].Qualified })
	return out
}

func avgDepth(cases []probeCase) int {
	if len(cases) == 0 {
		return 0
	}
	sum := 0
	for _, c := range cases {
		sum += c.Depth
	}
	return sum / len(cases)
}

func rate(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
