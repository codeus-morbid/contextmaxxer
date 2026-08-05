// Command confcal calibrates the confidence gate. It collects the top-1
// score distribution for queries whose answer EXISTS (gen corpus, split by
// whether the gold symbol was actually returned) and for queries whose answer
// does NOT exist (negprobe-style absent concepts), then reports, for every
// candidate threshold, what a gate at that threshold would cost and buy.
//
// Why absolute score: retrieval_health gates on the RELATIVE top1-top2 gap
// only, so a query with no real answer still reads "high" whenever one junk
// candidate happens to lead. The cross-encoder emits a sigmoid, so its score
// is comparable across queries — an absolute floor is the missing signal.
//
// Usage:
//
//	confcal -bin dist/contextmaxxer.exe -manifest internal/eval/testdata/manifest.public.json
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/eval"
	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	"github.com/codeus-morbid/contextmaxxer/internal/selfcases"
)

// Absent concepts, spot-checked against the corpus repos. Kept in sync with
// cmd/negprobe's built-in list.
var negatives = []string{
	"kafka consumer group rebalance listener",
	"stripe payment webhook signature verification",
	"graphql schema resolver for mutations",
	"kubernetes operator reconcile loop",
	"webrtc peer connection ice negotiation",
	"elasticsearch bulk indexing with retries",
	"blockchain wallet transaction signing",
	"smtp email delivery retry queue",
	"grpc bidirectional streaming handler",
	"saml single sign-on assertion parsing",
	"terraform state locking backend",
	"mqtt topic subscription with qos",
}

type sample struct {
	score float32
	hit   bool // gold present in the returned list (positives only)
}

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	manifestPath := flag.String("manifest", "internal/eval/testdata/manifest.public.json", "eval manifest")
	negPath := flag.String("negatives", "", "file with absent-concept queries (default: built-in list)")
	maxResults := flag.Int("max", 5, "max_results per call (the served default)")
	dump := flag.Bool("dump", false, "print every sample as score,class")
	index := flag.String("index", "", "calibrate on ONE index using docstring self-queries as positives (instead of the gen manifest)")
	nSelf := flag.Int("n", 120, "self-query positives to sample when -index is used")
	flag.Parse()

	if *negPath != "" {
		f, err := os.Open(*negPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		negatives = nil
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if q := strings.TrimSpace(sc.Text()); q != "" && !strings.HasPrefix(q, "#") {
				negatives = append(negatives, q)
			}
		}
		f.Close()
	}

	absBin, err := filepath.Abs(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var pos, neg []sample
	if *index != "" {
		pos, neg = calibrateIndex(absBin, *index, *nSelf, *maxResults)
		report(pos, neg, *dump)
		return
	}

	manifest, err := eval.LoadManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, proj := range manifest.Projects {
		cases, err := eval.LoadCases(proj.CasesPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", proj.Name, err)
			os.Exit(1)
		}
		srv, err := evalharness.Start(absBin, proj.IndexPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", proj.Name, err)
			os.Exit(1)
		}
		for _, c := range cases {
			res, err := srv.Find(c.Query, *maxResults)
			if err != nil || len(res.Scores) == 0 {
				continue
			}
			gold := make(map[string]bool, len(c.Expected))
			for _, e := range c.Expected {
				gold[e] = true
			}
			hit := false
			for _, n := range res.Names {
				if gold[n] {
					hit = true
					break
				}
			}
			pos = append(pos, sample{topRelevance(res), hit})
		}
		for _, q := range negatives {
			res, err := srv.Find(q, *maxResults)
			if err != nil || len(res.Scores) == 0 {
				continue
			}
			neg = append(neg, sample{topRelevance(res), false})
		}
		srv.Stop()
	}

	report(pos, neg, *dump)
}

// calibrateIndex collects positives from docstring self-queries (the answer
// provably exists — it is the symbol the docstring belongs to) and negatives
// from the absent-concept list, on one arbitrary index.
func calibrateIndex(absBin, indexPath string, n, maxResults int) (pos, neg []sample) {
	cases, _, err := selfcases.Sample(indexPath, selfcases.Options{N: n})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	absIndex, err := filepath.Abs(indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv, err := evalharness.Start(absBin, absIndex)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer srv.Stop()

	for _, c := range cases {
		res, err := srv.Find(c.Paraphrase(), maxResults)
		if err != nil || len(res.Scores) == 0 {
			continue
		}
		hit := false
		for _, name := range res.Names {
			if name == c.Qualified {
				hit = true
				break
			}
		}
		pos = append(pos, sample{topRelevance(res), hit})
	}
	for _, q := range negatives {
		res, err := srv.Find(q, maxResults)
		if err != nil || len(res.Scores) == 0 {
			continue
		}
		neg = append(neg, sample{topRelevance(res), false})
	}
	return pos, neg
}

func report(pos, neg []sample, dump bool) {
	if dump {
		for _, s := range pos {
			fmt.Printf("%.4f,pos,%v\n", s.score, s.hit)
		}
		for _, s := range neg {
			fmt.Printf("%.4f,neg,false\n", s.score)
		}
	}

	fmt.Printf("positives n=%d (answer exists)  negatives n=%d (answer absent)\n", len(pos), len(neg))
	printPercentiles("positives-hit", filter(pos, true))
	printPercentiles("positives-miss", filter(pos, false))
	printPercentiles("negatives", scores(neg))

	fmt.Println("\nthreshold sweep (gate: flag low-confidence when top1 score < T)")
	fmt.Printf("%-8s %-14s %-16s %s\n", "T", "neg caught", "pos-hit flagged", "pos-miss flagged")
	hits := filter(pos, true)
	misses := filter(pos, false)
	negScores := scores(neg)
	for _, t := range []float32{0.02, 0.05, 0.10, 0.15, 0.20, 0.30, 0.40, 0.50, 0.70} {
		fmt.Printf("%-8.2f %-14s %-16s %s\n", t,
			ratio(below(negScores, t), len(negScores)),
			ratio(below(hits, t), len(hits)),
			ratio(below(misses, t), len(misses)))
	}
}

func filter(ss []sample, hit bool) []float32 {
	var out []float32
	for _, s := range ss {
		if s.hit == hit {
			out = append(out, s.score)
		}
	}
	return out
}

func scores(ss []sample) []float32 {
	out := make([]float32, len(ss))
	for i, s := range ss {
		out[i] = s.score
	}
	return out
}

func below(vals []float32, t float32) int {
	n := 0
	for _, v := range vals {
		if v < t {
			n++
		}
	}
	return n
}

func ratio(a, n int) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d (%.2f)", a, n, float64(a)/float64(n))
}

func printPercentiles(label string, vals []float32) {
	if len(vals) == 0 {
		fmt.Printf("%-16s n=0\n", label)
		return
	}
	s := append([]float32(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) float32 {
		i := int(p * float64(len(s)-1))
		return s[i]
	}
	fmt.Printf("%-16s n=%-4d p05=%.3f p25=%.3f p50=%.3f p75=%.3f p95=%.3f\n",
		label, len(s), at(0.05), at(0.25), at(0.50), at(0.75), at(0.95))
}

// topRelevance is the cross-encoder's verdict on the best result — the only
// score not inflated by ranking bonuses, hence the only one comparable across
// queries. Falls back to -1 when the reranker did not run, so those samples
// can be told apart from genuinely weak matches instead of silently scoring 0.
func topRelevance(res evalharness.Result) float32 {
	if len(res.Relevance) == 0 || res.Relevance[0] == 0 {
		return -1
	}
	return res.Relevance[0]
}
