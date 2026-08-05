// Command idxstats prints symbol and edge counts for one or more index DBs.
// Handy for verifying extraction changes (e.g. type-aware callee resolution)
// actually changed the call-graph density: `idxstats a.db b.db`.
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: idxstats <index.db> [more.db ...]")
		os.Exit(2)
	}
	for _, path := range os.Args[1:] {
		s, e, err := counts(path)
		if err != nil {
			fmt.Printf("%-60s ERROR: %v\n", path, err)
			continue
		}
		fmt.Printf("%-60s symbols=%-8d edges=%-8d (edges/sym=%.2f)\n", path, s, e, ratio(e, s))
	}
}

func ratio(e, s int) float64 {
	if s == 0 {
		return 0
	}
	return float64(e) / float64(s)
}

func counts(path string) (symbols, edges int, err error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	if err := db.QueryRow("SELECT count(*) FROM symbols").Scan(&symbols); err != nil {
		return 0, 0, err
	}
	if err := db.QueryRow("SELECT count(*) FROM edges").Scan(&edges); err != nil {
		return 0, 0, err
	}
	return symbols, edges, nil
}
