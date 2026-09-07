// Command regionprobe asks where the gold sits relative to what we returned.
//
// The 848-instance run says HitFile 0.529 and HitRegion 0.375: having found the
// file, we point at the right code 71% of the time. CoSIL manages 100% and even
// BM25 manages 82%, so within-file targeting is our worst dimension against
// every baseline — and it is the one stage no ablation has ever varied.
//
// "Improve it" is not a plan until the miss is decomposed, because the three
// shapes it can take need opposite fixes:
//
//	trimmed   — we returned the right symbol and the evidence window cut the
//	            gold lines out of it. A packing problem.
//	wrong     — the right symbol was never in the answer. A selection problem,
//	            and the graph may know where it is: gold is what a solver READ,
//	            and solvers follow call edges.
//	unindexed — nothing in the index covers those lines. A corpus problem no
//	            ranking can reach.
//
// The split is measured against the SAME response, so the three cannot be
// confused with one another.
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	_ "modernc.org/sqlite"
)

type region struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type instance struct {
	InstanceID  string   `json:"instance_id"`
	Repo        string   `json:"repo"`
	Query       string   `json:"query"`
	GoldRegions []region `json:"gold_regions"`
}

// symbol is one indexed span, enough to locate a line and walk an edge.
type symbol struct {
	id         int64
	file       string
	start, end int
	name       string
}

func main() {
	manifestPath := flag.String("manifest", "manifest.jsonl", "instances with gold regions")
	reposDir := flag.String("repos", "repos", "extracted snapshots, one per instance_id")
	indexName := flag.String("index-name", "index.db", "index filename inside each snapshot")
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	maxResults := flag.Int("max", 20, "max_results per call (the run being explained used 20)")
	n := flag.Int("n", 0, "score at most this many instances (0 = all)")
	verbose := flag.Bool("v", false, "print every missed region")
	flag.Parse()

	instances, err := loadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}

	var (
		scored                            int
		total, hit, trimmed, wrong, unidx int
		wrongHop1, wrongHop2, wrongFar    int
		wrongSameFileRank                 int
		trimGaps, trimSymLines            []int
		trimInnerLines                    []int
		trimHadFiner                      int
		fileMissed                        int
	)

	for _, inst := range instances {
		if *n > 0 && scored >= *n {
			break
		}
		if len(inst.GoldRegions) == 0 {
			continue
		}
		root := filepath.Join(*reposDir, inst.InstanceID)
		indexPath := filepath.Join(root, ".contextmaxxer", *indexName)
		if _, err := os.Stat(indexPath); err != nil {
			continue
		}
		srv, err := evalharness.Start(*bin, indexPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: start: %v\n", inst.InstanceID, err)
			continue
		}
		res, err := srv.Find(inst.Query, *maxResults)
		srv.Stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: query: %v\n", inst.InstanceID, err)
			continue
		}
		scored++

		idx, err := openIndex(indexPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: index: %v\n", inst.InstanceID, err)
			continue
		}

		returnedFiles := map[string]bool{}
		returnedIDs := map[int64]bool{}
		for i, f := range res.Files {
			returnedFiles[norm(f)] = true
			// Map the returned symbol back to an indexed id by its span, so
			// graph distance can be walked from what we actually sent.
			if s, ok := idx.symbolAt(norm(f), spanStart(res.Lines[i])); ok {
				returnedIDs[s.id] = true
			}
		}

		for _, g := range inst.GoldRegions {
			total++
			gp := norm(g.Path)
			if !returnedFiles[gp] {
				fileMissed++
				continue
			}
			var overlapsVisible, overlapsFull bool
			for i, f := range res.Files {
				if norm(f) != gp {
					continue
				}
				if spansOverlap(res.Lines[i], g) {
					overlapsFull = true
				}
				vis := res.Visible[i]
				if vis == "" {
					vis = res.Lines[i]
				}
				if spansOverlap(vis, g) {
					overlapsVisible = true
				}
			}
			switch {
			case overlapsVisible:
				hit++
			case overlapsFull:
				trimmed++
				// A trimmed miss splits again, and the two halves need
				// different fixes: gold just outside the window is a width
				// problem, gold far from it is a choice problem.
				gap, symLines := trimGap(res, gp, g)
				trimGaps = append(trimGaps, gap)
				trimSymLines = append(trimSymLines, symLines)
				// Two explanations fit a 296-line median: the window was
				// pointed at the wrong part of a genuinely long function, or
				// the returned symbol was a container (a class, a module-level
				// block) and a finer symbol holding the gold existed all along.
				// The second is a granularity problem, not a windowing one.
				if inner, ok := idx.symbolAt(gp, g.Start); ok {
					innerLen := inner.end - inner.start
					trimInnerLines = append(trimInnerLines, innerLen)
					if symLines > 0 && innerLen*2 < symLines {
						trimHadFiner++
					}
				}
				if *verbose {
					fmt.Printf("  TRIMMED  %s %s:%d-%d gap=%d symbol=%d lines\n",
						inst.InstanceID, gp, g.Start, g.End, gap, symLines)
				}
			default:
				// The right symbol was not in the answer. Where is it?
				target, ok := idx.symbolAt(gp, g.Start)
				if !ok {
					unidx++
					if *verbose {
						fmt.Printf("  UNINDEXED %s %s:%d-%d\n", inst.InstanceID, gp, g.Start, g.End)
					}
					continue
				}
				wrong++
				switch d := idx.hops(target.id, returnedIDs); {
				case d == 1:
					wrongHop1++
				case d == 2:
					wrongHop2++
				default:
					wrongFar++
				}
				if idx.sameFileAsReturned(target, returnedFiles) {
					wrongSameFileRank++
				}
				if *verbose {
					fmt.Printf("  WRONG    %s %s:%d-%d -> %s (hops=%d)\n",
						inst.InstanceID, gp, g.Start, g.End, target.name, idx.hops(target.id, returnedIDs))
				}
			}
		}
		idx.close()
		if scored%25 == 0 {
			fmt.Fprintf(os.Stderr, "  %d instances...\n", scored)
		}
	}

	pct := func(a int) string {
		if total == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%5.1f%%", 100*float64(a)/float64(total))
	}
	fmt.Printf("\ninstances %d | gold regions %d\n\n", scored, total)
	fmt.Printf("  hit (a visible span covers it)      %5d  %s\n", hit, pct(hit))
	fmt.Printf("  file missed entirely                %5d  %s\n", fileMissed, pct(fileMissed))
	fmt.Printf("\n  --- file found, region missed ---\n")
	fmt.Printf("  trimmed: right symbol, cut window   %5d  %s\n", trimmed, pct(trimmed))
	fmt.Printf("  wrong symbol                        %5d  %s\n", wrong, pct(wrong))
	fmt.Printf("    1 call-graph hop from an answer   %5d  %s\n", wrongHop1, pct(wrongHop1))
	fmt.Printf("    2 hops                            %5d  %s\n", wrongHop2, pct(wrongHop2))
	fmt.Printf("    further or unreachable            %5d  %s\n", wrongFar, pct(wrongFar))
	fmt.Printf("  unindexed lines                     %5d  %s\n", unidx, pct(unidx))
	if len(trimGaps) > 0 {
		fmt.Printf("\n  --- the trimmed misses, in detail ---\n")
		summarize("lines gold sits away", trimGaps)
		summarize("length of that symbol", trimSymLines)
		near := 0
		for _, g := range trimGaps {
			if g >= 0 && g <= 10 {
				near++
			}
		}
		fmt.Printf("  within 10 lines of the shown window: %d of %d (%.0f%%)\n",
			near, len(trimGaps), 100*float64(near)/float64(len(trimGaps)))
		summarize("innermost symbol there", trimInnerLines)
		fmt.Printf("  a finer symbol existed: %d of %d (%.0f%%)  <- granularity, not windowing\n",
			trimHadFiner, len(trimGaps), 100*float64(trimHadFiner)/float64(len(trimGaps)))
	}

	fmt.Printf("\nEvery share is of all gold regions, so the column adds to 100%%.\n")
	fmt.Printf("A trimmed miss is a packing fix; a 1-hop miss is a selection fix the\ngraph already has the answer to; an unindexed miss is neither.\n")
}

// --- index access -----------------------------------------------------------

type indexView struct {
	db     *sql.DB
	byFile map[string][]symbol
	adj    map[int64][]int64
	loaded bool
}

func openIndex(path string) (*indexView, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	v := &indexView{db: db, byFile: map[string][]symbol{}, adj: map[int64][]int64{}}
	rows, err := db.Query(`SELECT s.id, f.path, s.start_line, s.end_line, s.qualified_name
	                       FROM symbols s JOIN files f ON f.id = s.file_id`)
	if err != nil {
		db.Close()
		return nil, err
	}
	for rows.Next() {
		var s symbol
		if err := rows.Scan(&s.id, &s.file, &s.start, &s.end, &s.name); err != nil {
			continue
		}
		s.file = norm(s.file)
		v.byFile[s.file] = append(v.byFile[s.file], s)
	}
	rows.Close()
	// Undirected: "one hop" means the two symbols are joined by a call edge in
	// either direction, which is what a response's caller/callee list offers.
	erows, err := db.Query(`SELECT src, dst FROM edges`)
	if err == nil {
		for erows.Next() {
			var a, b int64
			if erows.Scan(&a, &b) == nil {
				v.adj[a] = append(v.adj[a], b)
				v.adj[b] = append(v.adj[b], a)
			}
		}
		erows.Close()
	}
	for f := range v.byFile {
		// Innermost first, so a line inside a method resolves to the method
		// rather than the class that encloses it.
		sort.Slice(v.byFile[f], func(i, j int) bool {
			return (v.byFile[f][i].end - v.byFile[f][i].start) < (v.byFile[f][j].end - v.byFile[f][j].start)
		})
	}
	v.loaded = true
	return v, nil
}

func (v *indexView) close() {
	if v != nil && v.db != nil {
		v.db.Close()
	}
}

// symbolAt returns the smallest indexed symbol covering a line.
func (v *indexView) symbolAt(file string, line int) (symbol, bool) {
	for _, s := range v.byFile[file] {
		if s.start <= line && line <= s.end {
			return s, true
		}
	}
	return symbol{}, false
}

// hops is the call-graph distance from a target to the nearest returned symbol,
// capped at 3 because anything further is not a neighbour in any useful sense.
func (v *indexView) hops(target int64, returned map[int64]bool) int {
	if returned[target] {
		return 0
	}
	seen := map[int64]bool{target: true}
	frontier := []int64{target}
	for d := 1; d <= 2; d++ {
		var next []int64
		for _, id := range frontier {
			for _, nb := range v.adj[id] {
				if seen[nb] {
					continue
				}
				if returned[nb] {
					return d
				}
				seen[nb] = true
				next = append(next, nb)
			}
		}
		frontier = next
	}
	return 3
}

func (v *indexView) sameFileAsReturned(s symbol, files map[string]bool) bool {
	return files[s.file]
}

// --- small helpers ----------------------------------------------------------

func norm(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
}

// spanStart parses the "120-380" shape the response uses.
func spanStart(span string) int {
	a, _ := splitSpan(span)
	return a
}

func splitSpan(span string) (int, int) {
	i := strings.IndexByte(span, '-')
	if i < 0 {
		n, _ := strconv.Atoi(strings.TrimSpace(span))
		return n, n
	}
	a, _ := strconv.Atoi(strings.TrimSpace(span[:i]))
	b, _ := strconv.Atoi(strings.TrimSpace(span[i+1:]))
	return a, b
}

func spansOverlap(span string, g region) bool {
	a, b := splitSpan(span)
	if a == 0 && b == 0 {
		return false
	}
	return a <= g.End && g.Start <= b
}

func loadManifest(path string) ([]instance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []instance
	for sc.Scan() {
		var in instance
		if json.Unmarshal(sc.Bytes(), &in) != nil {
			continue
		}
		out = append(out, in)
	}
	return out, sc.Err()
}

// trimGap measures, for a region the evidence window cut away, how many lines
// separate the gold from the nearest visible window, and how long the symbol
// holding it is. A small gap says the window was too narrow; a large one inside
// a large symbol says the window was pointed at the wrong part.
func trimGap(res evalharness.Result, file string, g region) (gap, symLines int) {
	gap = 1 << 30
	for i, f := range res.Files {
		if norm(f) != file || !spansOverlap(res.Lines[i], g) {
			continue
		}
		a, b := splitSpan(res.Lines[i])
		if n := b - a; n > symLines {
			symLines = n
		}
		vis := res.Visible[i]
		if vis == "" {
			continue
		}
		va, vb := splitSpan(vis)
		switch {
		case g.Start > vb:
			if d := g.Start - vb; d < gap {
				gap = d
			}
		case g.End < va:
			if d := va - g.End; d < gap {
				gap = d
			}
		default:
			gap = 0
		}
	}
	if gap == 1<<30 {
		gap = -1 // no visible window on that symbol at all
	}
	return gap, symLines
}

func summarize(name string, xs []int) {
	if len(xs) == 0 {
		return
	}
	c := append([]int(nil), xs...)
	sort.Ints(c)
	q := func(p float64) int { return c[int(float64(len(c)-1)*p)] }
	fmt.Printf("  %-22s n=%d  median %d  p25 %d  p75 %d  max %d\n",
		name, len(c), q(0.5), q(0.25), q(0.75), c[len(c)-1])
}
