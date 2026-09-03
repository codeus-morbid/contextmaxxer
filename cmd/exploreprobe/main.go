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
	indexName := flag.String("index-name", "index.db", "index filename inside each snapshot .contextmaxxer dir; lets a second index (say, one built with another embedder) sit beside the first")
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	n := flag.Int("n", 0, "score at most this many instances (0 = all indexed ones)")
	dataset := flag.String("dataset", "", "restrict to one sub-dataset: verified|multilingual|pro")
	maxResults := flag.Int("max", 20, "max_results per call; the benchmark ranks a list, so this is the list")
	budget := flag.Int("budget", 500, "line budget B: score the longest prefix whose visible lines fit (0 = no budget). The benchmark reports every baseline at B=500")
	fullBodies := flag.Int("full", 0, "how many top results keep their full body (0 = served default of 3, -1 = all). The single biggest lever on how much code the answer contains")
	rerankK := flag.Int("rerank", 0, "cross-encoder pool size (0 = served default 15). Below max_results the tail of the response is never reranked")
	alpha := flag.String("alpha", "", "seed-vs-PageRank weight 0..1 (empty = served default; 1 = seeds only, which measures what the graph contributes)")
	skipIntent := flag.Bool("skip-intent", false, "disable the symbolic intent ranker")
	seedK := flag.Int("seed", 0, "seed candidates per channel before fusion (0 = pipeline default 20). The seed pool is the hard ceiling on reach: nothing downstream can return a file the seeds did not find")
	serverArgs := flag.String("server-args", "", "extra flags for the served mcp process, space separated, e.g. -reranker=none")
	only := flag.String("only", "", "score just this instance_id. A distributed worker deletes each snapshot after scoring it, so without this the scorer would re-walk every index still on disk")
	queryMode := flag.String("query-mode", "raw", "what to search for: raw (the whole issue report), title, ids (code identifiers), title+ids. Every ablation left HitFile unmoved, so the query itself is the untested stage")
	anchorExpand := flag.Int("anchor-expand", 0, "add this many 1-hop graph neighbours of the top seeds to the candidate pool (0 = off). 64.5%% of missed gold that IS indexed sits within 1-2 hops of something we returned")
	dumpFiles := flag.Bool("dump-files", false, "print instance, returned files and gold files as TSV, for offline analysis of what was missed")
	verbose := flag.Bool("v", false, "print every scored instance")
	csvOut := flag.Bool("csv", false, "print one machine-readable row per instance instead of a summary. Runs split across machines must be merged from these rows: averaging each machine's summary weights small shards equally with large ones")
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
		sumHitRegion, sumF1, sumNDCGB          float64
		sumKept, sumShown, sumUnique           int
		sumGoldViaRefs                         float64
		completeOneCall                        int
	)

	for _, inst := range instances {
		if *n > 0 && scored >= *n {
			break
		}
		if *only != "" && inst.InstanceID != *only {
			continue
		}
		if len(inst.GoldFiles) == 0 {
			skippedNoGold++
			continue
		}
		repoRoot := filepath.Join(*reposDir, inst.InstanceID)
		indexPath := filepath.Join(repoRoot, ".contextmaxxer", *indexName)
		if _, err := os.Stat(indexPath); err != nil {
			// Indexing 847 snapshots is hours of GPU; scoring whatever is
			// already indexed keeps this runnable on a subset.
			skippedNoIndex++
			continue
		}

		var extra []string
		if *serverArgs != "" {
			extra = strings.Fields(*serverArgs)
		}
		srv, err := evalharness.Start(*bin, indexPath, extra...)
		if err != nil {
			errored++
			fmt.Fprintf(os.Stderr, "%s: start: %v\n", inst.InstanceID, err)
			continue
		}
		if *fullBodies != 0 {
			srv.SetFullBodies(*fullBodies)
		}
		if *rerankK > 0 {
			srv.SetRerankK(*rerankK)
		}
		if *alpha != "" {
			srv.SetAlpha(*alpha)
		}
		if *skipIntent {
			srv.SetSkipIntent(true)
		}
		if *seedK > 0 {
			srv.SetSeedK(*seedK)
		}
		if *anchorExpand > 0 {
			srv.SetAnchorExpand(*anchorExpand)
		}
		res, err := srv.Find(shapeQuery(inst.Query, *queryMode), *maxResults)
		srv.Stop()
		if err != nil {
			errored++
			fmt.Fprintf(os.Stderr, "%s: query: %v\n", inst.InstanceID, err)
			continue
		}

		m := scoreWithBudget(inst, res, *budget)
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
		sumHitRegion += m.hitRegion
		sumF1 += m.f1
		sumNDCGB += m.ndcgB
		sumKept += m.kept
		sumShown += m.shownLines
		sumUnique += m.uniqueFiles
		sumGoldViaRefs += m.goldViaRefs
		if m.completeInOneCall {
			completeOneCall++
		}
		if *dumpFiles {
			// instance <TAB> returned files <TAB> gold files. Enough to ask,
			// offline, whether a missed gold file was reachable through the graph
			// from something we did return.
			returned := make([]string, 0, m.kept)
			for i := 0; i < m.kept && i < len(res.Files); i++ {
				returned = append(returned, normPath(res.Files[i]))
			}
			gold := make([]string, 0, len(inst.GoldFiles))
			for _, g := range inst.GoldFiles {
				gold = append(gold, normPath(g))
			}
			fmt.Printf("%s\t%s\t%s\n", inst.InstanceID, strings.Join(returned, ","), strings.Join(gold, ","))
		}
		switch {
		case *csvOut:
			fmt.Printf("%s,%s,%s,%.6f,%.6f,%.6f,%.6f,%.6f,%d,%.6f,%.6f,%.6f,%d,%d,%s,%.4f\n",
				inst.InstanceID, inst.Dataset, inst.Repo,
				m.hitFile, m.fileRecall, m.ndcg, m.lineRecall, m.efficiency,
				m.fuh, m.hitRegion, m.f1, m.ndcgB, m.kept, m.shownLines, m.confidence, m.topGap)
		case *verbose:
			fmt.Printf("%-52s hit=%.0f fileR=%.2f ndcg=%.2f lineR=%.2f eff=%.2f fuh=%d kept=%d lines=%d\n",
				inst.InstanceID, m.hitFile, m.fileRecall, m.ndcg, m.lineRecall, m.efficiency, m.fuh, m.kept, m.shownLines)
		}
	}

	if *csvOut {
		// The header goes last so the rows can be concatenated across machines
		// without stripping anything; it is a comment line.
		fmt.Fprintln(os.Stderr, "#instance_id,dataset,repo,hit,file_recall,ndcg,line_recall,efficiency,fuh,hit_region,f1,ndcg_b,kept,lines,confidence,top_gap")
		return
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

	// The paper's own column names, so the two tables can be read side by side.
	// Their HitFile is the SHARE of gold files reached (our File recall), and
	// their Prec is the share of returned lines that are gold (our Efficiency) —
	// the names collide with ours, which is why they are restated here.
	fmt.Printf("\n--- benchmark protocol, B=%d (paper's column names) ---\n", *budget)
	fmt.Printf("HitReg  %.3f | Prec %.3f | Rec_l %.3f | F1 %.3f | HitFile %.3f | nDCG@%d %.3f\n",
		sumHitRegion/f, sumEfficiency/f, sumLineRecall/f, sumF1/f, sumFileRecall/f, *budget, sumNDCGB/f)
	fmt.Printf("kept %.1f of %d results per instance, %.0f visible lines on average\n",
		float64(sumKept)/f, *maxResults, float64(sumShown)/f)
	fmt.Printf("distinct files %.1f of %.1f results — the rest are further symbols from a file already in the list\n",
		float64(sumUnique)/f, float64(sumKept)/f)

	// What ONE call puts within reach, counting the graph refs the response
	// already carries. This is the saved-calls figure: an agent that can follow
	// a ref does not pay for a second search.
	fmt.Printf("\n--- what one call reaches (results + graph refs) ---\n")
	fmt.Printf("gold within reach   %.3f   (returned outright: %.3f)\n", sumGoldViaRefs/f, sumFileRecall/f)
	fmt.Printf("no second search    %.3f   share of tasks where ALL gold is reachable from one call (%d of %d)\n",
		float64(completeOneCall)/f, completeOneCall, scored)
	fmt.Println("nDCG is our reading of their formula; Prec/Rec/HitFile are unambiguous.")
}

type metrics struct {
	hitFile, fileRecall, ndcg float64
	lineRecall, efficiency    float64
	fuh                       int

	// The benchmark's own protocol, computed over the in-budget prefix.
	hitRegion, f1, ndcgB float64
	kept, shownLines     int
	uniqueFiles          int
	// goldViaRefs is gold reachable from ONE call: results plus what their graph
	// refs point at. completeInOneCall means no second search is needed at all.
	goldViaRefs       float64
	confidence        string
	topGap            float32
	completeInOneCall bool
}

// score turns one response into the benchmark's metrics. Line-level numbers use
// the span the response ACTUALLY shows (visible_lines) and not the symbol's full
// extent: efficiency is a cost metric, and charging ourselves for lines we never
// sent would flatter it exactly where this tool trims hardest.
func score(inst instance, res evalharness.Result) metrics {
	return scoreWithBudget(inst, res, 0)
}

// scoreWithBudget scores only what fits the line budget, which is how the
// benchmark reports every published baseline. budget <= 0 scores the whole
// response.
func scoreWithBudget(inst instance, res evalharness.Result, budget int) metrics {
	gold := make(map[string]bool, len(inst.GoldFiles))
	for _, f := range inst.GoldFiles {
		gold[normPath(f)] = true
	}

	cut := budgetPrefix(res, budget)
	var m metrics
	m.kept = cut
	// How many DISTINCT files the returned symbols cover. The response ranks
	// symbols, but the benchmark scores files, so several results landing in one
	// file spend the list without widening reach — and that is the difference
	// between "rank better" and "diversify".
	distinct := map[string]bool{}
	for i := 0; i < cut; i++ {
		distinct[normPath(res.Files[i])] = true
	}
	m.uniqueFiles = len(distinct)

	// Saved calls. A response carries graph refs, and following one costs the
	// agent nothing — the next symbol is already on the page. So the honest
	// measure of "how many searches did this replace" is not how many queries an
	// agent happened to issue (that varied 57% between identical runs) but what
	// ONE call puts within reach: the files returned, plus the files its refs
	// point at.
	//
	// reachedFiles counts gold covered that way; completeInOneCall says the
	// agent never needs a second search for this task at all.
	reachable := make(map[string]bool, len(distinct))
	for f := range distinct {
		reachable[f] = true
	}
	for i := 0; i < cut && i < len(res.RefFiles); i++ {
		for _, rf := range res.RefFiles[i] {
			if rf != "" {
				reachable[normPath(rf)] = true
			}
		}
	}
	if len(gold) > 0 {
		hit := 0
		for g := range gold {
			if reachable[g] {
				hit++
			}
		}
		m.goldViaRefs = float64(hit) / float64(len(gold))
		m.completeInOneCall = hit == len(gold)
	}
	seen := map[string]bool{}
	var rels []float64
	for i, file := range res.Files[:cut] {
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
	shown := shownLines(res, cut)
	m.shownLines = len(shown)
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
		if m.lineRecall+m.efficiency > 0 {
			m.f1 = 2 * m.efficiency * m.lineRecall / (m.efficiency + m.lineRecall)
		}
	}
	m.confidence = res.Confidence
	m.topGap = res.TopGap
	m.hitRegion = hitRegion(inst, res, cut)
	m.ndcgB = ndcgBudget(inst, res, cut, budget)
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

func shownLines(res evalharness.Result, cut int) map[lineKey]bool {
	out := map[lineKey]bool{}
	for i, file := range res.Files[:cut] {
		for l := range spanLines(res, i) {
			out[lineKey{normPath(file), l}] = true
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
		inst.Query = decodeQuery(inst.Query)
		if dataset != "" && inst.Dataset != dataset {
			continue
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out, sc.Err()
}

// decodeQuery unwraps a query that the dataset stored as a JSON string INSIDE a
// JSON string. 103 of the 215 `pro` instances are shaped that way (and 2 of
// verified): after one decode the text still begins and ends with a quote and
// carries literal \n two characters wide. Searching that string means searching
// the escapes as well as the words.
//
// The unwrap is conditional on the value round-tripping as a JSON string, so a
// query that merely opens with a quotation mark is left alone.
func decodeQuery(q string) string {
	trimmed := strings.TrimSpace(q)
	if len(trimmed) < 2 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		return q
	}
	var inner string
	if err := json.Unmarshal([]byte(trimmed), &inner); err != nil {
		return q
	}
	return inner
}

// budgetPrefix returns how many results fit the benchmark's line budget: "the
// longest prediction prefix whose cumulative |L(·)| does not exceed B". It is a
// PREFIX, so the first result that does not fit ends the list — later, smaller
// ones are not squeezed in, because the agent reads the ranking in order.
//
// DECISION(2026-09): the budget is the benchmark's own protocol (B=500, also
// reported at 100 and 300) and without it our numbers cannot be put beside the
// published BM25/TF-IDF/agent baselines at all. It also changes the question
// being asked: returning twenty symbols regardless of size rewards recall that
// an agent would never read, while a budget scores what fits in the window it
// actually gets.
func budgetPrefix(res evalharness.Result, budget int) int {
	if budget <= 0 {
		return len(res.Files)
	}
	total := 0
	for i := range res.Files {
		n := len(spanLines(res, i))
		if total+n > budget {
			return i
		}
		total += n
	}
	return len(res.Files)
}

// spanLines is the set of lines one result actually shows.
func spanLines(res evalharness.Result, i int) map[int]bool {
	span := ""
	if i < len(res.Visible) {
		span = res.Visible[i]
	}
	if span == "" && i < len(res.Lines) {
		span = res.Lines[i]
	}
	out := map[int]bool{}
	for _, part := range strings.Split(span, ",") {
		start, end, ok := parseSpan(part)
		if !ok {
			continue
		}
		for l := start; l <= end; l++ {
			out[l] = true
		}
	}
	return out
}

// ndcgBudget implements the benchmark's ranking metric: DCG@B = sum over the
// in-budget prefix of g_i/log2(i+2), where g_i is the count of core lines that
// result i covers for the FIRST time.
//
// The ideal is the gold regions themselves, largest first, taken while they fit
// the same budget. That is our reading of "the best DCG attainable on the same
// instance under the same line budget"; the paper reports Oracle at 0.858
// rather than 1.000, so their ideal is normalised somewhat differently and this
// number should be treated as our approximation of their metric, unlike
// precision/recall/HitFile which are unambiguous.
func ndcgBudget(inst instance, res evalharness.Result, cut, budget int) float64 {
	goldLines := lineSet(inst.GoldRegions)
	covered := map[lineKey]bool{}
	var dcg float64
	for i := 0; i < cut; i++ {
		file := normPath(res.Files[i])
		gain := 0
		for line := range spanLines(res, i) {
			k := lineKey{file, line}
			if goldLines[k] && !covered[k] {
				covered[k] = true
				gain++
			}
		}
		dcg += float64(gain) / math.Log2(float64(i+2))
	}

	sizes := make([]int, 0, len(inst.GoldRegions))
	for _, r := range inst.GoldRegions {
		end := r.End
		if end-r.Start > 5000 {
			end = r.Start + 5000
		}
		if n := end - r.Start + 1; n > 0 {
			sizes = append(sizes, n)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sizes)))
	var idcg float64
	spent := 0
	for i, n := range sizes {
		if budget > 0 && spent+n > budget {
			break
		}
		spent += n
		idcg += float64(n) / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// hitRegion is the benchmark's "fraction of core regions for which the explorer
// surfaced at least one overlapping prediction".
func hitRegion(inst instance, res evalharness.Result, cut int) float64 {
	if len(inst.GoldRegions) == 0 {
		return 0
	}
	shown := map[lineKey]bool{}
	for i := 0; i < cut; i++ {
		file := normPath(res.Files[i])
		for line := range spanLines(res, i) {
			shown[lineKey{file, line}] = true
		}
	}
	hit := 0
	for _, r := range inst.GoldRegions {
		end := r.End
		if end-r.Start > 5000 {
			end = r.Start + 5000
		}
		for line := r.Start; line <= end; line++ {
			if shown[lineKey{normPath(r.Path), line}] {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(inst.GoldRegions))
}
