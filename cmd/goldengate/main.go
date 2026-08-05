// Command goldengate is THE regression gate: one command, one report, always
// the same definition of "our numbers", measured through the shipped binary.
//
// Run it before and after any change that could touch retrieval, and diff:
//
//	goldengate -bin dist/contextmaxxer.exe -out before.json
//	# ...change something, rebuild...
//	goldengate -bin dist/contextmaxxer.exe -out after.json -baseline before.json
//
// The diff marks every metric that moved more than the measured noise floor
// (±0.02 — a golden run is bit-stable within a machine session, but GPU float
// jitter across sessions can flip a near-tie at a rank boundary).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/codeus-morbid/contextmaxxer/internal/goldensuite"
)

const defaultConfigPath = "internal/eval/testdata/golden-suite.public.json"

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "served binary under test")
	cfgPath := flag.String("config", defaultConfigPath, "suite definition")
	out := flag.String("out", "", "write the report JSON here")
	baseline := flag.String("baseline", "", "compare against this report JSON")
	holdout := flag.Bool("holdout", false, "use the gen holdout split (burns a look)")
	noise := flag.Float64("noise", 0.02, "movement below this is reported as noise")
	quiet := flag.Bool("q", false, "suppress progress lines")
	flag.Parse()

	absBin, err := filepath.Abs(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, base, err := goldensuite.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := func(format string, args ...any) {
		if !*quiet {
			fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
		}
	}

	rep, errs := goldensuite.Run(absBin, cfg, base, *holdout, log)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "WARN %v\n", e)
	}

	printReport(rep)

	if *out != "" {
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *baseline != "" {
		data, err := os.ReadFile(*baseline)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var prev goldensuite.Report
		if err := json.Unmarshal(data, &prev); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if diffReport(&prev, rep, float32(*noise)) {
			// Real movement is not automatically a failure — it is a thing to
			// look at — so the exit code stays 0 and the caller decides.
			fmt.Println("\n(movement beyond the noise floor — inspect before promoting)")
		}
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
}

func printReport(r *goldensuite.Report) {
	if r.Gen != nil {
		fmt.Printf("gen corpus      n=%-4d Hit@1=%.2f Hit@3=%.2f R@5=%.2f R@10=%.2f  %dms\n",
			r.Gen.N, r.Gen.Hit1, r.Gen.Hit3, r.Gen.R5, r.Gen.R10, r.Gen.AvgMs)
	}
	// Hit@5 is printed strict/by-name: the gap is decl-vs-impl duplication
	// (the doc is on the interface, the code in the implementation), not a
	// retrieval failure.
	fmt.Printf("\n%-18s %-50s %s\n", "repo", "self-retrieval (paraphrase; Hit@5 strict/by-name)", "false confidence")
	for _, name := range sortedKeys(r.Repos) {
		res := r.Repos[name]
		self, neg := "-", "-"
		if res.Self != nil {
			self = fmt.Sprintf("n=%-4d Hit@1=%.2f Hit@5=%.2f/%.2f Hit@10=%.2f seed=%.2f",
				res.Self.N, res.Self.Hit1, res.Self.Hit5, res.Self.Hit5ByName,
				res.Self.Hit10, res.Self.SeedGap)
		}
		if res.Neg != nil {
			neg = fmt.Sprintf("%.2f (n=%d, %d dropped)", res.Neg.Rate, res.Neg.N, res.Neg.Dropped)
		}
		fmt.Printf("%-14s %-42s %s\n", name, self, neg)
	}
}

func diffReport(prev, cur *goldensuite.Report, noise float32) bool {
	fmt.Printf("\ndiff vs baseline (%s)\n", prev.Generated.Format("2006-01-02 15:04"))
	moved := false
	cmp := func(label string, was, now float64) {
		d := now - was
		if d == 0 {
			return
		}
		mark := "noise"
		if d > float64(noise) || d < -float64(noise) {
			mark = "MOVED"
			moved = true
		}
		fmt.Printf("  %-34s %.2f -> %.2f  (%+.2f %s)\n", label, was, now, d, mark)
	}
	if prev.Gen != nil && cur.Gen != nil {
		cmp("gen Hit@1", prev.Gen.Hit1, cur.Gen.Hit1)
		cmp("gen Hit@3", prev.Gen.Hit3, cur.Gen.Hit3)
		cmp("gen R@5", prev.Gen.R5, cur.Gen.R5)
		cmp("gen R@10", prev.Gen.R10, cur.Gen.R10)
	}
	for _, name := range sortedKeys(cur.Repos) {
		now, was := cur.Repos[name], prev.Repos[name]
		if now.Self != nil && was.Self != nil {
			cmp(name+" self Hit@1", was.Self.Hit1, now.Self.Hit1)
			cmp(name+" self Hit@5", was.Self.Hit5, now.Self.Hit5)
			cmp(name+" self Hit@5 by-name", was.Self.Hit5ByName, now.Self.Hit5ByName)
			cmp(name+" self seed-loss", was.Self.SeedGap, now.Self.SeedGap)
		}
		if now.Neg != nil && was.Neg != nil {
			cmp(name+" false-confidence", was.Neg.Rate, now.Neg.Rate)
		}
	}
	if !moved {
		fmt.Println("  (nothing beyond the noise floor)")
	}
	return moved
}

func sortedKeys(m map[string]goldensuite.RepoResult) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
