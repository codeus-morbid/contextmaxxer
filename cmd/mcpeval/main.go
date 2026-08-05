// Command mcpeval drives the gen-eval corpus through the REAL served stack —
// it spawns the shipped binary's `mcp` subcommand per project and talks
// JSON-RPC over stdio, exactly like an agent host does.
//
// DECISION(2026-07): cmd/eval measures the retrieval library; four
// eval-vs-served config divergences (intent ranker, rerank pool, adaptive,
// protect fix) lived undetected because nothing scored the served path.
// This harness closes that class: if the MCP server's ranking drifts from
// the library's, the numbers diverge here first.
//
// Usage:
//
//	mcpeval -bin dist/contextmaxxer.exe \
//	        -manifest internal/eval/testdata/manifest.public.json [-holdout]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/eval"
	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
)

type tally struct {
	n, hit1, hit3, r5, r10 int
	latency                time.Duration
}

func (t *tally) add(o tally) {
	t.n += o.n
	t.hit1 += o.hit1
	t.hit3 += o.hit3
	t.r5 += o.r5
	t.r10 += o.r10
	t.latency += o.latency
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n)
}

func scoreCase(c eval.QueryCase, ranked []string) (hit1, hit3, r5, r10 bool) {
	exp := make(map[string]bool, len(c.Expected))
	for _, e := range c.Expected {
		exp[e] = true
	}
	minHits := c.MinHits
	if minHits <= 0 {
		minHits = 1
	}
	countIn := func(k int) int {
		if k > len(ranked) {
			k = len(ranked)
		}
		n := 0
		for _, name := range ranked[:k] {
			if exp[name] {
				n++
			}
		}
		return n
	}
	// Mirrors cmd/eval: Hit@K is any-of; Recall@K honors min_hits.
	hit1 = countIn(1) >= 1
	hit3 = countIn(3) >= 1
	r5 = countIn(5) >= minHits
	r10 = countIn(10) >= minHits
	return
}

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	manifestPath := flag.String("manifest", "internal/eval/testdata/manifest.public.json", "eval manifest")
	holdout := flag.Bool("holdout", false, "use the holdout case files")
	maxResults := flag.Int("max", 10, "max_results per call (scores up to R@10)")
	verbose := flag.Bool("v", false, "print per-case expected ranks (for diffing runs)")
	project := flag.String("project", "", "run only this project from the manifest")
	flag.Parse()

	manifest, err := eval.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var global tally
	for _, proj := range manifest.Projects {
		if *project != "" && proj.Name != *project {
			continue
		}
		casesPath := proj.CasesPath
		if *holdout {
			casesPath = proj.CasesHoldoutPath
		}
		cases, err := eval.LoadCases(casesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", proj.Name, err)
			os.Exit(1)
		}

		srv, err := evalharness.Start(absBin, proj.IndexPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: start server: %v\n", proj.Name, err)
			os.Exit(1)
		}

		var t tally
		for _, c := range cases {
			t0 := time.Now()
			ranked, err := srv.FindContext(c.Query, *maxResults)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s/%s: %v\n", proj.Name, c.ID, err)
				srv.Stop()
				os.Exit(1)
			}
			t.latency += time.Since(t0)
			if *verbose {
				// Rank of each expected symbol (0 = not in returned list).
				ranks := make([]int, 0, len(c.Expected))
				for _, e := range c.Expected {
					rank := 0
					for i, name := range ranked {
						if name == e {
							rank = i + 1
							break
						}
					}
					ranks = append(ranks, rank)
				}
				fmt.Printf("  %s/%s ranks=%v\n", proj.Name, c.ID, ranks)
			}
			h1, h3, r5, r10 := scoreCase(c, ranked)
			t.n++
			if h1 {
				t.hit1++
			}
			if h3 {
				t.hit3++
			}
			if r5 {
				t.r5++
			}
			if r10 {
				t.r10++
			}
		}
		srv.Stop()

		fmt.Printf("%-15s n=%-3d Hit@1=%.2f Hit@3=%.2f R@5=%.2f R@10=%.2f p_avg=%dms\n",
			proj.Name, t.n, pct(t.hit1, t.n), pct(t.hit3, t.n), pct(t.r5, t.n), pct(t.r10, t.n),
			(t.latency / time.Duration(max(t.n, 1))).Milliseconds())
		global.add(t)
	}

	fmt.Printf("\nGLOBAL (served stack) n=%d Hit@1=%.2f Hit@3=%.2f R@5=%.2f R@10=%.2f\n",
		global.n, pct(global.hit1, global.n), pct(global.hit3, global.n),
		pct(global.r5, global.n), pct(global.r10, global.n))
}
