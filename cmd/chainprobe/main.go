// Command chainprobe measures whether the call graph makes a chain followable,
// without an agent in the loop.
//
// The agent A/B cannot answer this. Two identical runs of the same arm on the
// same repo came back 52.1K and 81.8K variable tokens — a 57% spread, wider
// than every between-repo difference the A/B was being read for. The spread
// comes from a discrete choice (how often the model asks for a full body), so
// more repeats would cost hours to shrink an error bar that a deterministic
// measurement does not have at all.
//
// What actually makes a chain cheap here is that the next hop arrives with the
// previous answer: a result carries its callers and callees, so following an
// edge costs no second search. So that is what this measures. For each
// consecutive pair in a hand-traced chain, it asks for the first symbol and
// checks whether the second is already on the page.
//
//	covered — the next hop appeared as a graph ref of the queried symbol
//	sibling — it appeared as another ranked result instead (still free to
//	          follow, but not because of the graph)
//	missed  — neither: the agent has to search again
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
)

type chain struct {
	name    string
	symbols []string
	// dispatch[i] marks the hop INTO symbols[i] as crossing a request boundary
	// or dynamic dispatch. No static call edge can exist there, so scoring the
	// graph for missing it measures the codebase, not the tool.
	dispatch []bool
}

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db")
	chainFile := flag.String("chains", "", "hand-traced chains, one symbol per line, blank line between chains")
	maxResults := flag.Int("max", 5, "max_results per call (the served default)")
	verbose := flag.Bool("v", false, "print every hop")
	flag.Parse()

	chains, err := loadChains(*chainFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chains:", err)
		os.Exit(1)
	}
	if len(chains) == 0 {
		fmt.Fprintln(os.Stderr, "no chains to probe")
		os.Exit(1)
	}

	srv, err := evalharness.Start(*bin, *indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start server:", err)
		os.Exit(1)
	}
	defer srv.Stop()

	var covered, sibling, missed, retrieved, hops, dispatchHops int
	for _, ch := range chains {
		for i := 0; i+1 < len(ch.symbols); i++ {
			from, to := ch.symbols[i], ch.symbols[i+1]
			if ch.dispatch[i+1] {
				// A KV request or a dynamic attribute call: there is no edge to
				// find, so this hop prices the codebase and not the graph.
				dispatchHops++
				if *verbose {
					fmt.Printf("  dispatch    %-46s -> %s (no static edge exists)\n", from, to)
				}
				continue
			}
			hops++
			res, err := srv.Find(identifierWords(from), *maxResults)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", from, err)
				os.Exit(1)
			}
			rank := indexOfName(res.Names, from)
			if rank < 0 {
				missed++
				if *verbose {
					fmt.Printf("  MISS-SOURCE %-46s -> %s (queried symbol not retrieved)\n", from, to)
				}
				continue
			}
			retrieved++
			switch {
			case hasName(res.Callers[rank], to) || hasName(res.Callees[rank], to):
				covered++
				if *verbose {
					fmt.Printf("  graph       %-46s -> %s\n", from, to)
				}
			case indexOfName(res.Names, to) >= 0:
				sibling++
				if *verbose {
					fmt.Printf("  sibling     %-46s -> %s\n", from, to)
				}
			default:
				missed++
				if *verbose {
					seen := append(append([]string{}, res.Callers[rank]...), res.Callees[rank]...)
					fmt.Printf("  MISSED      %-46s -> %-44s rank=%d saw=%d %v\n",
						from, to, rank+1, len(seen), seen)
				}
			}
		}
	}

	fmt.Printf("chains=%d hops=%d  source_retrieved=%.2f\n",
		len(chains), hops, rate(retrieved, hops))
	fmt.Printf("next hop on the page: graph=%.2f (%d)  sibling=%.2f (%d)  missed=%.2f (%d)\n",
		rate(covered, hops), covered, rate(sibling, hops), sibling, rate(missed, hops), missed)
	fmt.Printf("followable without a new search: %.2f\n", rate(covered+sibling, hops))
}

// indexOfName matches on the qualified name, tolerating the package prefix
// being spelled differently in the chain file than in the index.
func indexOfName(names []string, want string) int {
	for i, n := range names {
		if sameSymbol(n, want) {
			return i
		}
	}
	return -1
}

func hasName(names []string, want string) bool { return indexOfName(names, want) >= 0 }

func sameSymbol(a, b string) bool {
	if a == b {
		return true
	}
	// A chain may name "Replica.AdminTransferLease" where the index says
	// "kvserver.Replica.AdminTransferLease"; a suffix match on a dot boundary
	// is the same symbol, while a bare-name match would confuse namesakes.
	return strings.HasSuffix(a, "."+b) || strings.HasSuffix(b, "."+a)
}

// identifierWords turns a qualified name into the kind of phrase a caller
// would type. Querying the raw name would let exact lexical match do the work
// and measure something easier than the real task.
func identifierWords(qname string) string {
	var b strings.Builder
	prev := rune(0)
	for _, r := range qname {
		switch {
		case r == '.' || r == '_' || r == '/':
			b.WriteRune(' ')
		case unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)):
			b.WriteRune(' ')
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(unicode.ToLower(r))
		}
		prev = r
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func loadChains(path string) ([]chain, error) {
	if path == "" {
		return nil, fmt.Errorf("-chains is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []chain
	cur := chain{}
	flush := func() {
		if len(cur.symbols) > 1 {
			out = append(out, cur)
		}
		cur = chain{}
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "#"):
			if cur.name == "" {
				cur.name = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			}
		default:
			if strings.HasPrefix(line, "~") {
				cur.symbols = append(cur.symbols, strings.TrimSpace(line[1:]))
				cur.dispatch = append(cur.dispatch, true)
				break
			}
			cur.symbols = append(cur.symbols, line)
			cur.dispatch = append(cur.dispatch, false)
		}
	}
	flush()
	return out, sc.Err()
}

func rate(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
