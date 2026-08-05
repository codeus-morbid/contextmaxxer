// Command selfsweep is a label-free ranking radar for ANY indexed repo: every
// documented symbol must be findable by its own docstring. No hand-authored
// cases, so it runs at scale on repos we never wrote eval labels for
// (prometheus, cockroach, a beta user's private code) and its labels carry
// the repo authors' vocabulary, not the eval author's.
//
// Two modes per case, scored side by side:
//   - verbatim:   query = first docstring sentence as written
//   - paraphrase: same sentence with the leading symbol name stripped (Go doc
//     convention "Foo returns..." otherwise hands FTS the answer)
//
// Docstrings shared by more than one symbol are skipped (interface/impl pairs
// would be unfair misses under exact-symbol scoring).
//
// Usage:
//
//	selfsweep -bin dist/contextmaxxer.exe -index path/to/.contextmaxxer/index.db [-n 150]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	"github.com/codeus-morbid/contextmaxxer/internal/selfcases"
)

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db to sweep")
	n := flag.Int("n", 150, "number of symbols to sample (stride over all documented symbols)")
	minDoc := flag.Int("min-doc", 40, "minimum docstring length to qualify")
	kinds := flag.String("kinds", "function,method", "symbol kinds to sample (comma-separated; \"all\" disables the filter)")
	maxResults := flag.Int("max", 10, "max_results per call")
	deep := flag.Int("deep", 50, "re-query paraphrase misses with this window to classify them (0 disables)")
	seedK := flag.Int("seed-k", 0, "override the server's seed-pool size (0 = server default)")
	verbose := flag.Bool("v", false, "print per-case ranks for paraphrase misses")
	flag.Parse()

	cases, total, err := selfcases.Sample(*indexPath, selfcases.Options{N: *n, MinDoc: *minDoc, Kinds: *kinds})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "no documented symbols qualify")
		os.Exit(1)
	}

	absBin, err := filepath.Abs(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	absIndex, err := filepath.Abs(*indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv, err := evalharness.Start(absBin, absIndex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start server: %v\n", err)
		os.Exit(1)
	}
	defer srv.Stop()
	srv.SetSeedK(*seedK)

	type tally struct{ hit1, hit5, hit10, miss int }
	var verbatim, paraphrase tally
	var latency time.Duration
	var rankingLoss, seedLoss int
	score := func(t *tally, rank int) {
		switch {
		case rank == 1:
			t.hit1, t.hit5, t.hit10 = t.hit1+1, t.hit5+1, t.hit10+1
		case rank >= 2 && rank <= 5:
			t.hit5, t.hit10 = t.hit5+1, t.hit10+1
		case rank >= 6 && rank <= 10:
			t.hit10++
		default:
			t.miss++
		}
	}

	for _, c := range cases {
		t0 := time.Now()
		rankV, err := rankOf(srv, c.Sentence, c.Qualified, *maxResults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.Qualified, err)
			os.Exit(1)
		}
		stripped := c.Paraphrase()
		rankP, err := rankOf(srv, stripped, c.Qualified, *maxResults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.Qualified, err)
			os.Exit(1)
		}
		latency += time.Since(t0)
		score(&verbatim, rankV)
		score(&paraphrase, rankP)
		if rankP == 0 && *deep > *maxResults {
			// Classify the miss: reachable at a deeper window means the
			// candidate WAS retrieved and lost to ranking; absent means the
			// seed channels (vector + FTS) never surfaced it at all. The two
			// call for different levers, so never report a bare miss rate.
			deepRank, err := rankOf(srv, stripped, c.Qualified, *deep)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", c.Qualified, err)
				os.Exit(1)
			}
			if deepRank > 0 {
				rankingLoss++
			} else {
				seedLoss++
			}
			if *verbose {
				fmt.Printf("  MISS %-58s verbatim=%-2d deep=%-3d q=%q\n",
					c.Qualified, rankV, deepRank, truncate(stripped, 70))
			}
		} else if *verbose && rankP == 0 {
			fmt.Printf("  MISS %-58s verbatim=%d q=%q\n", c.Qualified, rankV, truncate(stripped, 70))
		}
	}

	nn := len(cases)
	fmt.Printf("index=%s documented=%d sampled=%d p_avg=%dms (2 calls/case)\n",
		filepath.Base(filepath.Dir(filepath.Dir(absIndex))), total, nn,
		(latency / time.Duration(nn)).Milliseconds())
	report := func(label string, t tally) {
		fmt.Printf("%-10s Hit@1=%.2f Hit@5=%.2f Hit@10=%.2f miss=%.2f\n",
			label, float64(t.hit1)/float64(nn), float64(t.hit5)/float64(nn),
			float64(t.hit10)/float64(nn), float64(t.miss)/float64(nn))
	}
	report("verbatim", verbatim)
	report("paraphrase", paraphrase)
	if rankingLoss+seedLoss > 0 {
		fmt.Printf("miss split  ranking=%d (in top-%d, lost to ranking) seed=%d (never retrieved)\n",
			rankingLoss, *deep, seedLoss)
	}
}

func rankOf(srv *evalharness.Server, query, qualified string, maxResults int) (int, error) {
	ranked, err := srv.FindContext(query, maxResults)
	if err != nil {
		return 0, err
	}
	for i, name := range ranked {
		if name == qualified {
			return i + 1, nil
		}
	}
	return 0, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
