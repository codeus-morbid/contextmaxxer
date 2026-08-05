// Command negprobe measures the false-confidence rate: queries about
// plausible concepts that do NOT exist in the indexed repo. A healthy stack
// flags these low-confidence (retrieval_health present); answering them with
// a confident-looking symbol list teaches the agent to trust junk.
//
// This is the metric none of the positive-label suites can see: gen-eval,
// selfsweep and giteval all ask about things that exist.
//
// Negatives are verified absent PER REPO before they count (see
// internal/negcases) — an unverified list measures the list, not the tool.
//
// Usage:
//
//	negprobe -bin dist/contextmaxxer.exe -index path/to/index.db [-v]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	"github.com/codeus-morbid/contextmaxxer/internal/negcases"
)

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db to probe")
	queriesPath := flag.String("queries", "", "file with one absent-concept query per line (skips verification)")
	verbose := flag.Bool("v", false, "print each query's verdict and top-1")
	showDropped := flag.Bool("show-dropped", false, "list candidates rejected because the concept exists here")
	flag.Parse()

	absIndex, err := filepath.Abs(*indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var queries []string
	var dropped map[string]string
	if *queriesPath != "" {
		f, err := os.Open(*queriesPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if q := strings.TrimSpace(sc.Text()); q != "" && !strings.HasPrefix(q, "#") {
				queries = append(queries, q)
			}
		}
		f.Close()
	} else {
		valid, drop, err := negcases.Verify(absIndex, negcases.Default)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		dropped = drop
		for _, c := range valid {
			queries = append(queries, c.Query)
		}
	}
	if len(queries) == 0 {
		fmt.Fprintln(os.Stderr, "no valid negatives for this index (every candidate concept exists here)")
		os.Exit(1)
	}

	absBin, err := filepath.Abs(*bin)
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

	falseConfident := 0
	for _, q := range queries {
		res, err := srv.Find(q, 5)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%q: %v\n", q, err)
			os.Exit(1)
		}
		flagged := res.Confidence != ""
		if !flagged {
			falseConfident++
		}
		if *verbose {
			top1 := "-"
			if len(res.Names) > 0 {
				top1 = res.Names[0]
			}
			verdict := "flagged-low"
			if !flagged {
				verdict = "CONFIDENT"
			}
			fmt.Printf("  %-11s top1=%-50s q=%q\n", verdict, top1, q)
		}
	}
	if *showDropped {
		for q, term := range dropped {
			fmt.Printf("  dropped (%q present) %q\n", term, q)
		}
	}
	n := len(queries)
	fmt.Printf("index=%s negatives=%d (verified; %d candidates dropped as present) false_confident=%d rate=%.2f\n",
		filepath.Base(filepath.Dir(filepath.Dir(absIndex))), n, len(dropped), falseConfident,
		float64(falseConfident)/float64(n))
}
