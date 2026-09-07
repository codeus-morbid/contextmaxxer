// Command reachprobe asks why a gold file is never retrieved.
//
// The ceiling decomposition leaves two retrieval gaps of similar size: 13.1% of
// gold files sit in the response below rank 20 (a ranking problem) and 14.3% are
// not retrieved even among two hundred results. The second is the one no lever
// has touched, and the fix depends entirely on WHY the file is invisible:
//
//	semantic  — the query's vocabulary appears nowhere in the file. Only a
//	            better representation reaches this: a stronger embedder, or
//	            generated queries per document.
//	lexical   — the words ARE in the indexed text and the lexical channel still
//	            ranked the file out of two hundred. Term weighting or expansion.
//	uncovered — the words are in the file but NOT in what we index, because the
//	            body cap truncated them or no symbol covers that span. Neither
//	            ranking nor a better model reaches this; the indexer does.
//
// Every share is reported against a control — the same statistic over the gold
// files we DID retrieve — because "identifiers appear in 60% of missed files"
// means nothing until you know the figure for the found ones.
package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	_ "modernc.org/sqlite"
)

type instance struct {
	InstanceID string   `json:"instance_id"`
	Query      string   `json:"query"`
	GoldFiles  []string `json:"gold_files"`
}

// Same shapes cmd/exploreprobe extracts from an issue.
var (
	reBackticked = regexp.MustCompile("`([^`\n]{2,80})`")
	reCamel      = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z0-9]*\b|\b[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]*\b`)
	reSnake      = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
)

func stop(s string) bool {
	switch strings.ToLower(s) {
	case "e.g", "i.e", "etc", "vs", "self", "true", "false", "none", "null",
		"the", "and", "for", "with", "this", "that", "when", "then", "should",
		"github.com", "readme.md", "python", "javascript":
		return true
	}
	return false
}

func identifiers(text string, limit int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.Trim(s, "`.,;:()[]{}\"'")
		if len(s) < 4 || len(s) > 60 || seen[strings.ToLower(s)] || stop(s) {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	for _, m := range reBackticked.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, re := range []*regexp.Regexp{reSnake, reCamel} {
		for _, m := range re.FindAllString(text, -1) {
			add(m)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func norm(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
}

// tally is one population of gold files: the retrieved ones or the missed ones.
type tally struct {
	files      int
	inFile     int // an identifier appears in the file on disk
	inIndexed  int // an identifier appears in the text we actually index
	onlyOnDisk int // in the file but not in the indexed text
}

func (t tally) line(name string) string {
	if t.files == 0 {
		return fmt.Sprintf("  %-28s (none)", name)
	}
	p := func(a int) float64 { return 100 * float64(a) / float64(t.files) }
	return fmt.Sprintf("  %-28s n=%4d | in file %5.1f%% | in indexed text %5.1f%% | only on disk %5.1f%%",
		name, t.files, p(t.inFile), p(t.inIndexed), p(t.onlyOnDisk))
}

func main() {
	manifestPath := flag.String("manifest", "manifest.jsonl", "instances with gold files")
	reposDir := flag.String("repos", "repos", "extracted snapshots")
	indexName := flag.String("index-name", "index.db", "index filename inside each snapshot")
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	deep := flag.Int("deep", 200, "how many results count as 'retrieved at all'")
	n := flag.Int("n", 0, "score at most this many instances (0 = all)")
	ftsRankMode := flag.Bool("fts-rank", false, "also ask FTS, with the identifiers alone, where it would put each gold file")
	flag.Parse()
	if *ftsRankMode {
		ftsRanks = &rankPair{}
	}

	instances, err := loadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}

	var found, missed tally
	var notIndexed, scored, noIdents int

	for _, inst := range instances {
		if *n > 0 && scored >= *n {
			break
		}
		if len(inst.GoldFiles) == 0 {
			continue
		}
		root := filepath.Join(*reposDir, inst.InstanceID)
		indexPath := filepath.Join(root, ".contextmaxxer", *indexName)
		if _, err := os.Stat(indexPath); err != nil {
			continue
		}
		ids := identifiers(inst.Query, 8)
		if len(ids) == 0 {
			noIdents++
			continue
		}

		// The reranker only reorders; switching it off keeps a deep run cheap
		// and cannot change which files are reachable at all.
		srv, err := evalharness.Start(*bin, indexPath, "-reranker", "none")
		if err != nil {
			continue
		}
		res, err := srv.Find(inst.Query, *deep)
		srv.Stop()
		if err != nil {
			continue
		}
		scored++

		retrieved := map[string]bool{}
		for _, f := range res.Files {
			retrieved[norm(f)] = true
		}
		indexedText, indexedFiles, err := indexedTextByFile(indexPath)
		if err != nil {
			continue
		}

		for _, g := range inst.GoldFiles {
			gp := norm(g)
			if !indexedFiles[gp] {
				notIndexed++
				continue
			}
			t := &missed
			if retrieved[gp] {
				t = &found
			}
			t.files++

			onDisk := containsAny(readFile(filepath.Join(root, gp)), ids)
			inIdx := containsAny(indexedText[gp], ids)
			if onDisk {
				t.inFile++
			}
			if inIdx {
				t.inIndexed++
			}
			if onDisk && !inIdx {
				t.onlyOnDisk++
			}
			// The lever this probe is deciding: for a file the lexical channel
			// COULD match, where would a query of the identifiers alone put it?
			// Asked of FTS directly rather than through the pipeline, so the
			// vector channel and the graph cannot answer for it.
			if inIdx && ftsRanks != nil {
				r := ftsRankOf(indexPath, ids, gp)
				if retrieved[gp] {
					ftsRanks.found = append(ftsRanks.found, r)
				} else {
					ftsRanks.missed = append(ftsRanks.missed, r)
				}
			}
		}
		if scored%25 == 0 {
			fmt.Fprintf(os.Stderr, "  %d instances...\n", scored)
		}
	}

	fmt.Printf("\ninstances %d (skipped, no identifiers in the issue: %d)\n", scored, noIdents)
	fmt.Printf("gold files not in the index at all: %d (a corpus question, excluded below)\n\n", notIndexed)
	fmt.Println(found.line("RETRIEVED (the control)"))
	fmt.Println(missed.line("NEVER RETRIEVED"))
	if ftsRanks != nil {
		fmt.Println("\nWhere a query of the identifiers ALONE puts the file, asked of FTS directly:")
		reportRanks("retrieved (control)", ftsRanks.found)
		reportRanks("never retrieved", ftsRanks.missed)
	}
	fmt.Printf(`
Read the two lines against each other, not alone.
  missed "in file" near the control  -> the words are there and we still miss
                                        the file: a lexical/weighting problem.
  missed "in file" far below         -> the query's vocabulary is absent: only a
                                        better representation reaches it.
  "only on disk" high                -> the words exist but are outside what we
                                        index (body cap, uncovered spans): an
                                        indexer problem, not a model one.
`)
}

// indexedTextByFile returns, per file, the concatenation of everything the
// index actually holds for it — the same text the lexical channels search.
func indexedTextByFile(path string) (map[string]string, map[string]bool, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT f.path, s.qualified_name, s.signature, s.docstring, s.body_excerpt
	                       FROM symbols s JOIN files f ON f.id = s.file_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	text := map[string]*strings.Builder{}
	files := map[string]bool{}
	for rows.Next() {
		var p, qn, sig, doc, body string
		if rows.Scan(&p, &qn, &sig, &doc, &body) != nil {
			continue
		}
		p = norm(p)
		files[p] = true
		b := text[p]
		if b == nil {
			b = &strings.Builder{}
			text[p] = b
		}
		b.WriteString(qn)
		b.WriteByte('\n')
		b.WriteString(sig)
		b.WriteByte('\n')
		b.WriteString(doc)
		b.WriteByte('\n')
		b.WriteString(body)
		b.WriteByte('\n')
	}
	out := make(map[string]string, len(text))
	for p, b := range text {
		out[p] = b.String()
	}
	return out, files, nil
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func containsAny(haystack string, ids []string) bool {
	for _, id := range ids {
		if strings.Contains(haystack, id) {
			return true
		}
	}
	return false
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

// rankPair holds where a pure identifier query lands the gold file, for the
// files we retrieved and for the ones we did not.
type rankPair struct{ found, missed []int }

var ftsRanks *rankPair

// ftsRankOf asks the index's own FTS, with a query of nothing but the code
// identifiers, at what position the gold file first appears. -1 means it does
// not appear within the scanned depth.
//
// The query is built the way internal/store/sqlite builds one — each term
// quoted, joined with OR — so this measures the channel as it is, not an
// idealized version of it.
func ftsRankOf(indexPath string, ids []string, gold string) int {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(indexPath)+"?mode=ro")
	if err != nil {
		return -1
	}
	defer db.Close()

	var b strings.Builder
	for i, id := range ids {
		if !singleToken(id) {
			continue
		}
		if b.Len() > 0 {
			_ = i
			b.WriteString(" OR ")
		}
		b.WriteByte('"')
		b.WriteString(id)
		b.WriteByte('"')
	}
	if b.Len() == 0 {
		return -1
	}
	const depth = 500
	for _, q := range []string{
		`SELECT f.path FROM symbol_fts x JOIN symbols s ON s.id = x.rowid
		 JOIN files f ON f.id = s.file_id WHERE symbol_fts MATCH ? ORDER BY rank LIMIT ?`,
		`SELECT f.path FROM symbol_body_fts x JOIN symbols s ON s.id = x.rowid
		 JOIN files f ON f.id = s.file_id WHERE symbol_body_fts MATCH ? ORDER BY rank LIMIT ?`,
	} {
		rows, err := db.Query(q, b.String(), depth)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		pos := 0
		for rows.Next() {
			var p string
			if rows.Scan(&p) != nil {
				continue
			}
			p = norm(p)
			if seen[p] {
				continue // rank the FILE, not each of its symbols
			}
			seen[p] = true
			pos++
			if p == gold {
				rows.Close()
				return pos
			}
		}
		rows.Close()
	}
	return -1
}

func singleToken(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return s != ""
}

func reportRanks(name string, xs []int) {
	if len(xs) == 0 {
		fmt.Printf("  %-26s (none)\n", name)
		return
	}
	var in20, in50, absent int
	var present []int
	for _, x := range xs {
		switch {
		case x < 0:
			absent++
		default:
			present = append(present, x)
			if x <= 20 {
				in20++
			}
			if x <= 50 {
				in50++
			}
		}
	}
	sort.Ints(present)
	med := 0
	if len(present) > 0 {
		med = present[len(present)/2]
	}
	p := func(a int) float64 { return 100 * float64(a) / float64(len(xs)) }
	fmt.Printf("  %-26s n=%3d | top-20 %5.1f%% | top-50 %5.1f%% | median rank %4d | absent %5.1f%%\n",
		name, len(xs), p(in20), p(in50), med, p(absent))
}
