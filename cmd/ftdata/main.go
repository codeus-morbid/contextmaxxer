// Command ftdata turns CORE-Bench-style BEIR data into embedder fine-tuning
// triplets: {"query", "pos": [...], "neg": [...]} JSONL, one line per query.
//
// Methodology guardrails baked in:
//   - The train/holdout split is BY REPOSITORY, decided deterministically
//     (fnv1a(repoKey) % 100 < holdout-pct) plus a forced holdout list — so
//     the model is never trained on any repo it will be evaluated on, and
//     re-running ftdata reproduces the exact same split. The split is written
//     to split.json next to the data; commit that file with any training run.
//   - Hard negatives are mined per query INSIDE the query's temporal filter
//     (filtered_corpus_id): top BM25-ranked chunks that are not relevant.
//     BM25 mining needs no GPU/model, so it can run while a benchmark owns
//     the GPU; vector mining can be added later from the embedding caches.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/benchdata"
)

type triplet struct {
	Query string   `json:"query"`
	Pos   []string `json:"pos"`
	Neg   []string `json:"neg"`
	Meta  struct {
		Repo    string `json:"repo"`
		QueryID string `json:"query_id"`
	} `json:"meta"`
}

type splitManifest struct {
	HoldoutPct    int      `json:"holdout_pct"`
	ForcedHoldout []string `json:"forced_holdout"`
	Train         []string `json:"train"`
	Holdout       []string `json:"holdout"`
	Negs          int      `json:"negs_per_query"`
	MaxPos        int      `json:"max_pos_per_query"`
	MaxChars      int      `json:"max_chars"`
}

func main() { os.Exit(run()) }

func run() int {
	dataRoot := flag.String("data", "", "root dir; every subdir with corpus.jsonl is a repo")
	outDir := flag.String("out", "", "output dir for train.jsonl + split.json")
	negs := flag.Int("negs", 8, "hard negatives per query")
	maxPos := flag.Int("max-pos", 4, "max positives per query (extra relevant chunks dropped)")
	maxChars := flag.Int("max-chars", 4000, "truncate query/chunk texts to this many bytes")
	holdoutPct := flag.Int("holdout-pct", 15, "percent of repos held out (deterministic by repo-name hash)")
	forceHoldout := flag.String("force-holdout", "", "comma-separated repo dir names always held out (e.g. the eval-subset repos)")
	flag.Parse()

	if *dataRoot == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: ftdata -data <dir> -out <dir> [-negs 8] [-force-holdout a,b,c]")
		return 2
	}
	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "out dir: %v\n", err)
		return 1
	}

	forced := map[string]bool{}
	for _, name := range strings.Split(*forceHoldout, ",") {
		if name = strings.TrimSpace(name); name != "" {
			forced[name] = true
		}
	}

	var repos []string
	filepath.WalkDir(*dataRoot, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "corpus.jsonl" {
			repos = append(repos, filepath.Dir(path))
		}
		return nil
	})
	sort.Strings(repos)
	if len(repos) == 0 {
		fmt.Fprintf(os.Stderr, "no corpus.jsonl under %s\n", *dataRoot)
		return 1
	}

	manifest := splitManifest{
		HoldoutPct: *holdoutPct, Negs: *negs, MaxPos: *maxPos, MaxChars: *maxChars,
	}
	for name := range forced {
		manifest.ForcedHoldout = append(manifest.ForcedHoldout, name)
	}
	sort.Strings(manifest.ForcedHoldout)

	trainPath := filepath.Join(*outDir, "train.jsonl")
	out, err := os.Create(trainPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create train.jsonl: %v\n", err)
		return 1
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	totalTriplets, totalSkipped := 0, 0
	for _, repoDir := range repos {
		key := repoKey(*dataRoot, repoDir)
		if isHoldout(key, filepath.Base(repoDir), forced, *holdoutPct) {
			manifest.Holdout = append(manifest.Holdout, key)
			continue
		}
		manifest.Train = append(manifest.Train, key)

		n, skipped, err := emitRepo(enc, repoDir, key, *negs, *maxPos, *maxChars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", key, err)
			return 1
		}
		totalTriplets += n
		totalSkipped += skipped
		fmt.Printf("%-60s triplets=%d skipped=%d\n", key, n, skipped)
	}

	mf, err := os.Create(filepath.Join(*outDir, "split.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create split.json: %v\n", err)
		return 1
	}
	me := json.NewEncoder(mf)
	me.SetIndent("", "  ")
	if err := me.Encode(manifest); err != nil {
		mf.Close()
		fmt.Fprintf(os.Stderr, "write split.json: %v\n", err)
		return 1
	}
	mf.Close()

	fmt.Printf("\ntrain repos=%d holdout repos=%d triplets=%d skipped=%d\n-> %s\n",
		len(manifest.Train), len(manifest.Holdout), totalTriplets, totalSkipped, trainPath)
	return 0
}

// isHoldout: forced names always hold out; otherwise a deterministic hash of
// the repo key decides, so the split never depends on run order or flags.
func isHoldout(key, base string, forced map[string]bool, pct int) bool {
	if forced[base] {
		return true
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32()%100) < pct
}

func emitRepo(enc *json.Encoder, repoDir, key string, negs, maxPos, maxChars int) (emitted, skipped int, err error) {
	docs, err := benchdata.LoadCorpus(filepath.Join(repoDir, "corpus.jsonl"), maxChars)
	if err != nil {
		return 0, 0, fmt.Errorf("corpus: %w", err)
	}
	queries, err := benchdata.LoadQueries(filepath.Join(repoDir, "queries.jsonl"), maxChars)
	if err != nil {
		return 0, 0, fmt.Errorf("queries: %w", err)
	}
	qrels, err := benchdata.LoadQrels(filepath.Join(repoDir, "qrels", "test.tsv"))
	if err != nil {
		return 0, 0, fmt.Errorf("qrels: %w", err)
	}

	idx := make(map[string]int, len(docs))
	for i, d := range docs {
		idx[d.ID] = i
	}
	bm := benchdata.NewBM25Index(docs)

	for _, q := range queries {
		rel := qrels[q.ID]
		if len(rel) == 0 {
			skipped++
			continue
		}

		// Candidates respect the query's temporal filter.
		var cand []int
		if len(q.Filtered) == 0 {
			cand = make([]int, len(docs))
			for i := range cand {
				cand[i] = i
			}
		} else {
			cand = make([]int, 0, len(q.Filtered))
			for _, id := range q.Filtered {
				if row, ok := idx[id]; ok {
					cand = append(cand, row)
				}
			}
		}

		var t triplet
		t.Query = q.Text
		t.Meta.Repo = key
		t.Meta.QueryID = q.ID

		// Positives: relevant chunks, deterministic order, capped.
		posIDs := make([]string, 0, len(rel))
		for id := range rel {
			posIDs = append(posIDs, id)
		}
		sort.Strings(posIDs)
		for _, id := range posIDs {
			row, ok := idx[id]
			if !ok {
				continue
			}
			t.Pos = append(t.Pos, docs[row].Text)
			if len(t.Pos) >= maxPos {
				break
			}
		}
		if len(t.Pos) == 0 {
			skipped++
			continue
		}

		// Hard negatives: best BM25-ranked non-relevant candidates.
		for _, row := range bm.Rank(q.Text, cand) {
			if rel[docs[row].ID] > 0 {
				continue
			}
			t.Neg = append(t.Neg, docs[row].Text)
			if len(t.Neg) >= negs {
				break
			}
		}
		if len(t.Neg) == 0 {
			skipped++
			continue
		}

		if err := enc.Encode(&t); err != nil {
			return emitted, skipped, fmt.Errorf("write triplet: %w", err)
		}
		emitted++
	}
	return emitted, skipped, nil
}

func repoKey(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}
