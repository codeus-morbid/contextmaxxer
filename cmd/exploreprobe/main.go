// Command exploreprobe scores the served retrieval against SWE-Explore, the
// first external benchmark that grades the WHOLE pipeline rather than its
// entrance.
//
// CORE-Bench already covers the seed stage — see BENCHMARK.md — but it feeds
// its own chunked corpus, so PageRank, the intent ranker, the evidence windows
// and the packer are downstream of anything it can see. Its labels also come
// from git diffs: what a developer CHANGED. SWE-Explore labels what strong
// agents READ before they succeeded, intersected across runs and audited by
// hand, which is nearer to what this tool is for — we do not predict the patch,
// we hand the agent what it has to read first.
//
// The benchmark's own framing is that "sparse retrievers, interactive agents,
// and long-context selectors are all compared as producers of the same ranked
// region list", so a standalone search tool plugs in directly: every result is
// a (file, start, end) tuple, which is exactly a find_context symbol.
//
// Data (CC-BY-NC-ND: run it and publish numbers, do not redistribute):
//
//	huggingface.co/datasets/SWE-Explore-Bench/SWE-Explore-Bench
//
// Usage:
//
//	exploreprobe -manifest manifest.jsonl -repos repos [-n 30] [-dataset verified]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
)

type region struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type instance struct {
	InstanceID  string   `json:"instance_id"`
	Dataset     string   `json:"dataset"`
	Repo        string   `json:"repo"`
	SHA         string   `json:"sha"`
	Query       string   `json:"query"`
	GoldFiles   []string `json:"gold_files"`
	GoldRegions []region `json:"gold_regions"`
}

func main() {
	manifestPath := flag.String("manifest", "manifest.jsonl", "instances joined with their queries and gold")
	reposDir := flag.String("repos", "repos", "directory of extracted snapshots, one per instance_id")
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	n := flag.Int("n", 0, "score at most this many instances (0 = all indexed ones)")
	dataset := flag.String("dataset", "", "restrict to one sub-dataset: verified|multilingual|pro")
	maxResults := flag.Int("max", 20, "max_results per call; the benchmark ranks a list, so this is the list")
	verbose := flag.Bool("v", false, "print every scored instance")
	flag.Parse()

	instances, err := loadManifest(*manifestPath, *dataset)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}

	var (
		scored                                 int
		skippedNoIndex, skippedNoGold, errored int
		sumHitFile, sumFileRecall, sumNDCG     float64
		sumLineRecall, sumEfficiency           float64
		sumFUH                                 float64
		fuhCount                               int
	)

	for _, inst := range instances {
		if *n > 0 && scored >= *n {
			break
		}
		if len(inst.GoldFiles) == 0 {
			skippedNoGold++
			continue
		}
		repoRoot := filepath.Join(*reposDir, inst.InstanceID)
		indexPath := filepath.Join(repoRoot, ".contextmaxxer", "index.db")
		if _, err := os.Stat(indexPath); err != nil {
			// Indexing 847 snapshots is hours of GPU; scoring whatever is
			// already indexed keeps this runnable on a subset.
			skippedNoIndex++
			continue
		}

		srv, err := evalharness.Start(*bin, indexPath)
		if err != nil {
			errored++
			fmt.Fprintf(os.Stderr, "%s: start: %v\n", inst.InstanceID, err)
			continue
		}
		res, err := srv.Find(inst.Query, *maxResults)
		srv.Stop()
		if err != nil {
			errored++
			fmt.Fprintf(os.Stderr, "%s: query: %v\n", inst.InstanceID, err)
			continue
		}

		m := score(inst, res)
		scored++
		sumHitFile += m.hitFile
		sumFileRecall += m.fileRecall
		sumNDCG += m.ndcg
		sumLineRecall += m.lineRecall
		sumEfficiency += m.efficiency
		if m.fuh > 0 {
			sumFUH += float64(m.fuh)
			fuhCount++
		}
		if *verbose {
			fmt.Printf("%-52s hit=%.0f fileR=%.2f ndcg=%.2f lineR=%.2f eff=%.2f fuh=%d\n",
				inst.InstanceID, m.hitFile, m.fileRecall, m.ndcg, m.lineRecall, m.efficiency, m.fuh)
		}
	}

	if scored == 0 {
		fmt.Printf("nothing scored (no index: %d, no gold: %d, errors: %d)\n",
			skippedNoIndex, skippedNoGold, errored)
		fmt.Println("build an index per snapshot first: contextmaxxer index <repos>/<instance_id>")
		return
	}

	f := float64(scored)
	fmt.Printf("\nscored=%d  (skipped: no index %d, no gold %d; errors %d)  max_results=%d\n",
		scored, skippedNoIndex, skippedNoGold, errored, *maxResults)
	fmt.Printf("HitFile        %.3f   at least one gold file in the returned list\n", sumHitFile/f)
	fmt.Printf("File recall    %.3f   share of gold files returned\n", sumFileRecall/f)
	fmt.Printf("nDCG@%-3d      %.3f   ranking quality over file relevance\n", *maxResults, sumNDCG/f)
	fmt.Printf("Line recall    %.3f   share of gold LINES covered by what we returned\n", sumLineRecall/f)
	fmt.Printf("Efficiency     %.3f   share of returned lines that are gold — the cost side\n", sumEfficiency/f)
	if fuhCount > 0 {
		fmt.Printf("First useful   %.2f   mean rank of the first gold file (over %d instances that hit)\n",
			sumFUH/float64(fuhCount), fuhCount)
	}
}

type metrics struct {
	hitFile, fileRecall, ndcg float64
	lineRecall, efficiency    float64
	fuh                       int
}

// score turns one response into the benchmark's metrics. Line-level numbers use
// the span the response ACTUALLY shows (visible_lines) and not the symbol's full
// extent: efficiency is a cost metric, and charging ourselves for lines we never
// sent would flatter it exactly where this tool trims hardest.
func score(inst instance, res evalharness.Result) metrics {
	gold := make(map[string]bool, len(inst.GoldFiles))
	for _, f := range inst.GoldFiles {
		gold[normPath(f)] = true
	}

	var m metrics
	seen := map[string]bool{}
	var rels []float64
	for i, file := range res.Files {
		file = normPath(file)
		rel := 0.0
		if gold[file] {
			m.hitFile = 1
			if m.fuh == 0 {
				m.fuh = i + 1
			}
			// Only the FIRST result from a gold file is relevant. A response
			// carries one entry per symbol, so twenty results routinely cover two
			// files; crediting every repeat pushed DCG past an IDCG computed over
			// unique files and produced nDCG of 1.56 on the first real instance.
			// A file already returned is already found — finding it again is not
			// additional relevance.
			if !seen[file] {
				seen[file] = true
				rel = 1
			}
		}
		rels = append(rels, rel)
	}
	if len(gold) > 0 {
		m.fileRecall = float64(len(seen)) / float64(len(gold))
	}
	m.ndcg = ndcg(rels, len(gold))

	goldLines := lineSet(inst.GoldRegions)
	shown := shownLines(res)
	if len(goldLines) > 0 {
		var covered int
		for k := range shown {
			if goldLines[k] {
				covered++
			}
		}
		m.lineRecall = float64(covered) / float64(len(goldLines))
		if len(shown) > 0 {
			m.efficiency = float64(covered) / float64(len(shown))
		}
	}
	return m
}

type lineKey struct {
	file string
	line int
}

func lineSet(regions []region) map[lineKey]bool {
	out := map[lineKey]bool{}
	for _, r := range regions {
		// A region of thousands of lines is a whole file read; it still counts,
		// but the cap keeps one such gold entry from drowning every other
		// instance in the average.
		end := r.End
		if end-r.Start > 5000 {
			end = r.Start + 5000
		}
		for l := r.Start; l <= end; l++ {
			out[lineKey{normPath(r.Path), l}] = true
		}
	}
	return out
}

func shownLines(res evalharness.Result) map[lineKey]bool {
	out := map[lineKey]bool{}
	for i, file := range res.Files {
		span := ""
		if i < len(res.Visible) {
			span = res.Visible[i]
		}
		if span == "" && i < len(res.Lines) {
			span = res.Lines[i]
		}
		for _, part := range strings.Split(span, ",") {
			start, end, ok := parseSpan(part)
			if !ok {
				continue
			}
			for l := start; l <= end; l++ {
				out[lineKey{normPath(file), l}] = true
			}
		}
	}
	return out
}

// parseSpan reads "120-380"; the response also uses comma-separated spans when
// evidence selection shows several windows of one symbol.
func parseSpan(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "-")
	if i <= 0 {
		return 0, 0, false
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(s[:i]))
	end, err2 := strconv.Atoi(strings.TrimSpace(s[i+1:]))
	if err1 != nil || err2 != nil || end < start {
		return 0, 0, false
	}
	return start, end, true
}

// normPath makes the two sides comparable: the benchmark writes POSIX paths
// relative to the repo root, and an index built on Windows reports backslashes.
func normPath(p string) string {
	return strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
}

func ndcg(rels []float64, idealHits int) float64 {
	var dcg float64
	for i, r := range rels {
		dcg += r / math.Log2(float64(i+2))
	}
	ideal := idealHits
	if ideal > len(rels) {
		ideal = len(rels)
	}
	var idcg float64
	for i := 0; i < ideal; i++ {
		idcg += 1 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func loadManifest(path, dataset string) ([]instance, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []instance
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // issue texts are long
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var inst instance
		if err := json.Unmarshal([]byte(line), &inst); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if dataset != "" && inst.Dataset != dataset {
			continue
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out, sc.Err()
}
