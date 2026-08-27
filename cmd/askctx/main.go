// Command askctx runs one find_context against a chosen binary and prints the
// response an agent host would receive.
//
// It exists because MCP servers are spawned once per host session: an agent A/B
// inside a running session measures whichever binary was current when the
// session started, which after a day of rebuilds is not the one under test.
// This pays a process start per call — report tokens and accuracy from it,
// never wall time.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
)

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "binary to serve the request")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db")
	maxResults := flag.Int("max", 5, "max_results")
	mode := flag.String("mode", "", "output mode: answer (default), minimal, explore")
	full := flag.Bool("full", false, "return whole bodies instead of query-relevant windows (stands in for expand_context)")
	flag.Parse()

	query := flag.Arg(0)
	if query == "" {
		fmt.Fprintln(os.Stderr, `usage: askctx [-index db] [-max n] "your question"`)
		os.Exit(2)
	}

	srv, err := evalharness.Start(*bin, *indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start server:", err)
		os.Exit(1)
	}
	defer srv.Stop()

	var out string
	if *full {
		out, err = srv.FindMarkdownFull(query, *maxResults, *mode)
	} else {
		out, err = srv.FindMarkdown(query, *maxResults, *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	fmt.Println(out)
}
