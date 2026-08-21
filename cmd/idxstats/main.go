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
		fmt.Printf("%-60s symbols=%-8d edges=%-8d (edges/sym=%.2f) content=v%s\n",
			path, s, e, ratio(e, s), contentVersion(path))
		if t, err := truncation(path); err == nil {
			fmt.Printf("%-60s truncated=%-8d (%.1f%% of symbols)  visible=%dKB hidden=%dKB (%.1f%% of their code unsearchable)\n",
				"", t.n, 100*ratio(t.n, s), t.visible/1024, t.hidden/1024,
				100*ratio(t.hidden, t.visible+t.hidden))
			if t.chunks > 0 {
				fmt.Printf("%-60s chunks=%-8d (%.1f per capped symbol)\n",
					"", t.chunks, ratio(t.chunks, t.n))
			}
			if t.bodyFTS > 0 {
				fmt.Printf("%-60s body_fts=%dKB (%.1f%% of the body text it indexes)\n",
					"", t.bodyFTS/1024, 100*ratio(t.bodyFTS, t.visible+t.hidden))
			}
		}
	}
}

// truncStats measures how much symbol code never reaches the retrieval
// channels: vector, FTS and rerank all read body_excerpt, which the extractor
// caps (bodyExcerptLimit). symbol_bodies holds the full body for exactly the
// symbols that were capped, so the delta is the unsearchable remainder.
type truncStats struct{ n, visible, hidden, bodyFTS, chunks int }

func truncation(path string) (truncStats, error) {
	var t truncStats
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return t, err
	}
	defer db.Close()
	err = db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(LENGTH(s.body_excerpt)), 0),
		       COALESCE(SUM(LENGTH(b.body) - LENGTH(s.body_excerpt)), 0)
		FROM symbol_bodies b JOIN symbols s ON s.id = b.symbol_id`,
	).Scan(&t.n, &t.visible, &t.hidden)
	if err != nil {
		return t, err
	}
	// The body index is external-content, so this is index overhead only — the
	// bodies themselves are not copied into it.
	_ = db.QueryRow(`SELECT COALESCE(SUM(LENGTH(block)), 0) FROM symbol_body_fts_data`).Scan(&t.bodyFTS)
	_ = db.QueryRow(`SELECT COUNT(*) FROM symbol_chunks`).Scan(&t.chunks)
	return t, nil
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

// contentVersion reports the extractor generation that filled the index. v1
// predates lossless bodies and exact call lines, and the server refuses to
// serve it — the only cure is a rebuild.
func contentVersion(path string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "?"
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM _meta WHERE key='index_content_version'`).Scan(&v); err != nil || v == "" {
		return "?"
	}
	return v
}
