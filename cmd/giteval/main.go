// Command giteval scores the served stack against labels nobody on this
// project authored: each case is a real commit — query = the commit subject,
// gold = the symbols whose enclosing-function hunk headers appear in that
// commit's diff, resolved against the index. If the tool can take the words a
// developer used to DESCRIBE a change and surface the symbols the change
// touched, it works on real questions about real data.
//
// Label independence caveat: commit subjects describe deltas ("fix X when Y"),
// agent queries describe code ("where is X decided") — related but not the
// same distribution. Treat absolute numbers as a floor, deltas as the signal.
//
// Usage:
//
//	giteval -bin .task/build/contextmaxxer -repo ../public-project [-scan 400] [-n 100]
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	_ "modernc.org/sqlite"
)

type gitCase struct {
	hash, query string
	gold        []string // qualified names
}

var (
	identRe  = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	prefixRe = regexp.MustCompile(`^[a-z]+(\([^)]*\))?[:!]\s*`) // fix(scope): ...
	issueRe  = regexp.MustCompile(`\(?#\d+\)?`)
)

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	repo := flag.String("repo", ".", "git repo root (index at <repo>/.contextmaxxer/index.db)")
	indexPath := flag.String("index", "", "index db (default <repo>/.contextmaxxer/index.db)")
	scan := flag.Int("scan", 400, "commits to scan for candidate cases")
	n := flag.Int("n", 100, "max cases to run (newest first)")
	maxResults := flag.Int("max", 10, "max_results per call")
	verbose := flag.Bool("v", false, "print per-case best rank")
	flag.Parse()

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	idx := *indexPath
	if idx == "" {
		idx = filepath.Join(absRepo, ".contextmaxxer", "index.db")
	}

	cases, scanned, err := extractCases(absRepo, idx, *scan, *n)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintf(os.Stderr, "no usable commits among %d scanned (shallow clone? trivial subjects?)\n", scanned)
		os.Exit(1)
	}

	absBin, err := filepath.Abs(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv, err := evalharness.Start(absBin, idx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start server: %v\n", err)
		os.Exit(1)
	}
	defer srv.Stop()

	var hit1, hit3, r5, r10, nn int
	var latency time.Duration
	for _, c := range cases {
		t0 := time.Now()
		ranked, err := srv.FindContext(c.query, *maxResults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.hash[:8], err)
			os.Exit(1)
		}
		latency += time.Since(t0)
		best := 0
		gold := make(map[string]bool, len(c.gold))
		for _, g := range c.gold {
			gold[g] = true
		}
		for i, name := range ranked {
			if gold[name] {
				best = i + 1
				break
			}
		}
		nn++
		if best >= 1 && best <= 1 {
			hit1++
		}
		if best >= 1 && best <= 3 {
			hit3++
		}
		if best >= 1 && best <= 5 {
			r5++
		}
		if best >= 1 && best <= 10 {
			r10++
		}
		if *verbose {
			fmt.Printf("  %s best=%-2d gold=%d q=%q\n", c.hash[:8], best, len(c.gold), truncate(c.query, 70))
		}
	}

	fmt.Printf("repo=%s scanned=%d cases=%d p_avg=%dms\n", filepath.Base(absRepo), scanned, nn,
		(latency / time.Duration(max(nn, 1))).Milliseconds())
	fmt.Printf("git-history  Hit@1=%.2f Hit@3=%.2f Hit@5=%.2f Hit@10=%.2f\n",
		pct(hit1, nn), pct(hit3, nn), pct(r5, nn), pct(r10, nn))
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n)
}

// extractCases walks `git log -p -U0` newest-first and builds cases from
// commits whose hunk-header context identifiers resolve to indexed symbols.
func extractCases(repo, indexPath string, scan, limit int) ([]gitCase, int, error) {
	db, err := sql.Open("sqlite", "file:"+indexPath+"?mode=ro")
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	// The index stores OS-native separators (backslashes on Windows); git
	// always emits forward slashes — normalize the index side in SQL.
	lookup, err := db.Prepare(`
		SELECT s.qualified_name FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE replace(f.path, char(92), '/') = ? AND s.name = ?`)
	if err != nil {
		return nil, 0, err
	}
	defer lookup.Close()

	// The index root may sit below the git root (repo/subproj/.contextmaxxer);
	// git paths are repo-root-relative, index paths are index-root-relative.
	prefix := ""
	if topBytes, err := exec.Command("git", "-C", repo, "rev-parse", "--show-toplevel").Output(); err == nil {
		top := strings.TrimSpace(string(topBytes))
		absRepo, _ := filepath.Abs(repo)
		if rel, err := filepath.Rel(top, absRepo); err == nil && rel != "." {
			prefix = strings.ReplaceAll(rel, "\\", "/") + "/"
		}
	}

	cmd := exec.Command("git", "-C", repo, "log", "--no-merges", "-n", fmt.Sprint(scan),
		"--format=\x01%H\x1f%s", "-p", "-U0", "--diff-filter=M")
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, fmt.Errorf("git log: %w", err)
	}

	var cases []gitCase
	var cur *gitCase
	scanned := 0
	curFile := ""
	goldSet := map[string]bool{}
	flush := func() {
		if cur == nil {
			return
		}
		if len(goldSet) >= 1 && len(goldSet) <= 8 && len(strings.Fields(cur.query)) >= 4 && !isCompound(cur.query) {
			for g := range goldSet {
				cur.gold = append(cur.gold, g)
			}
			cases = append(cases, *cur)
		}
		cur, curFile, goldSet = nil, "", map[string]bool{}
	}

	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "\x01"):
			flush()
			if len(cases) >= limit {
				return cases, scanned, nil
			}
			parts := strings.SplitN(strings.TrimPrefix(line, "\x01"), "\x1f", 2)
			if len(parts) != 2 {
				continue
			}
			scanned++
			q := cleanSubject(parts[1])
			cur = &gitCase{hash: parts[0], query: q}
		case strings.HasPrefix(line, "diff --git "):
			curFile = ""
			// "diff --git a/path b/path" — take the b/ side.
			if i := strings.Index(line, " b/"); i >= 0 {
				curFile = strings.TrimPrefix(line[i+3:], prefix)
			}
		case strings.HasPrefix(line, "@@") && cur != nil && curFile != "":
			// "@@ -a,b +c,d @@ <enclosing declaration>"
			ctxStart := strings.Index(line[2:], "@@")
			if ctxStart < 0 {
				continue
			}
			context := strings.TrimSpace(line[2+ctxStart+2:])
			for _, ident := range identRe.FindAllString(context, -1) {
				if len(ident) < 3 || isKeyword(ident) {
					continue
				}
				rows, err := lookup.Query(curFile, ident)
				if err != nil {
					continue
				}
				for rows.Next() {
					var qn string
					if rows.Scan(&qn) == nil {
						goldSet[qn] = true
					}
				}
				rows.Close()
			}
		}
	}
	flush()
	return cases, scanned, nil
}

func cleanSubject(s string) string {
	s = prefixRe.ReplaceAllString(s, "")
	s = issueRe.ReplaceAllString(s, "")
	// "pkg/kv: do the thing" — the path prefix is a hint a real user would
	// not type; drop it.
	if i := strings.Index(s, ": "); i > 0 && !strings.Contains(s[:i], " ") {
		s = s[i+2:]
	}
	return strings.TrimSpace(s)
}

// isCompound filters multi-topic subjects ("fix A, add B + C") — a single
// agent query never bundles unrelated changes, so such cases measure subject
// style, not retrieval.
func isCompound(s string) bool {
	return strings.Count(s, ",") >= 2 || strings.Contains(s, " + ") ||
		strings.Count(s, ";") >= 1
}

func isKeyword(s string) bool {
	switch s {
	case "func", "def", "class", "type", "struct", "interface", "const", "var",
		"static", "public", "private", "protected", "async", "export", "return":
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
