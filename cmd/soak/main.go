// Command soak asks one long-lived server the same questions over and over and
// checks that the answers do not drift.
//
// It exists because a defect was found where results depend on process state
// rather than on the index: the same MATCH returned 489 rows from a clean
// process and 240 from one with the ONNX runtime loaded, and some queries
// failed outright with "database disk image is malformed" on a database that
// passes integrity_check. Whatever the mechanism, the observable is a served
// answer that changes without the index changing — which no correctness suite
// looks for, because they all ask each question once.
//
// So this asks each question many times in ONE server, the way an agent session
// does, and reports the first round where an answer stops matching round 1.
//
// Usage:
//
//	soak -bin dist/contextmaxxer.exe -index path/to/index.db [-rounds 20] [-queries file]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
)

// defaultQueries includes "index", the term that reproduces the CLI failure, so
// a soak run always exercises the known-bad case alongside ordinary ones.
var defaultQueries = []string{
	"index", "table", "cache", "buffer", "error handling", "config parsing",
	"write path", "read path", "how are results ranked", "what happens on startup",
	"where is the retry logic", "compaction", "serialize a request",
	"validate user input", "background worker loop",
}

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db")
	rounds := flag.Int("rounds", 20, "how many times to ask the whole set")
	maxResults := flag.Int("max", 5, "max_results per call (the served default)")
	queryFile := flag.String("queries", "", "questions, one per line (default: a built-in set)")
	flag.Parse()

	queries := defaultQueries
	if *queryFile != "" {
		loaded, err := readLines(*queryFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "queries:", err)
			os.Exit(1)
		}
		queries = append(queries, loaded...)
	}

	srv, err := evalharness.Start(*bin, *indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start server:", err)
		os.Exit(1)
	}
	defer srv.Stop()

	baseline := make([][]string, len(queries))
	var calls, errCount, drift int

	for round := 1; round <= *rounds; round++ {
		for qi, q := range queries {
			calls++
			res, err := srv.Find(q, *maxResults)
			if err != nil {
				errCount++
				fmt.Printf("ERROR   round=%d q=%q: %v\n", round, q, err)
				continue
			}
			if round == 1 {
				baseline[qi] = res.Names
				continue
			}
			if baseline[qi] == nil {
				baseline[qi] = res.Names
				continue
			}
			if !sameNames(baseline[qi], res.Names) {
				drift++
				fmt.Printf("DRIFT   round=%d q=%q\n  round1: %v\n  now:    %v\n",
					round, q, baseline[qi], res.Names)
			}
		}
	}

	fmt.Printf("\nqueries=%d rounds=%d calls=%d errors=%d drift=%d\n",
		len(queries), *rounds, calls, errCount, drift)
	if errCount > 0 || drift > 0 {
		os.Exit(1)
	}
	fmt.Println("stable: every answer matched round 1")
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, sc.Err()
}
