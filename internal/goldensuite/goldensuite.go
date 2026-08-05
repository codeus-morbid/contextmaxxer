// Package goldensuite is the repeatable regression gate: one definition of
// "our numbers", run against the shipped binary, comparable across runs.
//
// It combines the three measurements that run everywhere and stay stable:
//   - gen corpus (hand-authored labels, the target metrics)
//   - self-retrieval sweep (label-free, any repo, the repo authors' words)
//   - verified negatives (false confidence — invisible to the other two)
//
// giteval is deliberately NOT included: its label quality depends on how deep
// the clone's history is, so it is a manual tool, not a gate.
package goldensuite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/eval"
	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	"github.com/codeus-morbid/contextmaxxer/internal/negcases"
	"github.com/codeus-morbid/contextmaxxer/internal/selfcases"
)

// Config declares what the suite covers. Paths are relative to the config
// file, so the checked-in definition works on any machine that has the repos.
type Config struct {
	GenManifest string      `json:"gen_manifest"`
	Repos       []RepoEntry `json:"repos"`
	// SelfN is how many self-retrieval cases to sample per repo.
	SelfN int `json:"self_n"`
	// SkipIntent turns the intent ranker off for the whole run (A/B knob).
	SkipIntent bool `json:"skip_intent,omitempty"`
}

type RepoEntry struct {
	Name  string `json:"name"`
	Index string `json:"index"`
}

// Report is one full run, JSON-serializable so runs can be diffed.
type Report struct {
	Generated time.Time             `json:"generated"`
	Gen       *GenResult            `json:"gen,omitempty"`
	Repos     map[string]RepoResult `json:"repos"`
}

type GenResult struct {
	N     int     `json:"n"`
	Hit1  float64 `json:"hit1"`
	Hit3  float64 `json:"hit3"`
	R5    float64 `json:"r5"`
	R10   float64 `json:"r10"`
	AvgMs int64   `json:"avg_ms"`
}

type RepoResult struct {
	Self *SelfResult `json:"self,omitempty"`
	Neg  *NegResult  `json:"neg,omitempty"`
}

type SelfResult struct {
	N     int     `json:"n"`
	Hit1  float64 `json:"hit1"`
	Hit5  float64 `json:"hit5"`
	Hit10 float64 `json:"hit10"`
	Miss  float64 `json:"miss"`
	// Hit5ByName accepts any symbol with the same short name as the target.
	// DECISION(2026-07): interface/impl pairs make exact-symbol scoring
	// unfair — the docstring lives on the trait declaration while the code
	// lives in the implementing class, and returning the impl IS the answer
	// an agent needs. os-lib read 0.23 "never retrieved" purely because
	// BasePathImpl.baseName was scored as a miss for BasePath.baseName.
	// Strict stays primary (it is the honest lower bound); this is the
	// upper bound, and a wide gap means decl/impl duplication, not failure.
	Hit5ByName float64 `json:"hit5_by_name"`
	SeedGap    float64 `json:"seed_loss_rate"`
	AvgMs      int64   `json:"avg_ms"`
}

type NegResult struct {
	N              int     `json:"n"`
	Dropped        int     `json:"dropped_as_present"`
	FalseConfident int     `json:"false_confident"`
	Rate           float64 `json:"rate"`
}

func LoadConfig(path string) (*Config, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, "", err
	}
	if c.SelfN == 0 {
		c.SelfN = 120
	}
	return &c, filepath.Dir(path), nil
}

// Run executes the whole suite. Component failures are reported per component
// rather than aborting: a missing repo must not hide the metrics that did run.
func Run(bin string, cfg *Config, base string, holdout bool, log func(string, ...any)) (*Report, []error) {
	var errs []error
	rep := &Report{Generated: time.Now().UTC(), Repos: map[string]RepoResult{}}

	if cfg.GenManifest != "" {
		log("gen corpus...")
		g, err := runGen(bin, filepath.Join(base, cfg.GenManifest), holdout, cfg.SkipIntent)
		if err != nil {
			errs = append(errs, fmt.Errorf("gen: %w", err))
		} else {
			rep.Gen = g
		}
	}

	for _, r := range cfg.Repos {
		index := r.Index
		if !filepath.IsAbs(index) {
			index = filepath.Join(base, index)
		}
		if _, err := os.Stat(index); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
			continue
		}
		var out RepoResult
		log("%s: self-retrieval...", r.Name)
		if s, err := runSelf(bin, index, cfg.SelfN, cfg.SkipIntent); err != nil {
			errs = append(errs, fmt.Errorf("%s self: %w", r.Name, err))
		} else {
			out.Self = s
		}
		log("%s: negatives...", r.Name)
		if n, err := runNeg(bin, index); err != nil {
			errs = append(errs, fmt.Errorf("%s neg: %w", r.Name, err))
		} else {
			out.Neg = n
		}
		rep.Repos[r.Name] = out
	}
	return rep, errs
}

func runGen(bin, manifestPath string, holdout bool, skipIntent bool) (*GenResult, error) {
	manifest, err := eval.LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	var n, hit1, hit3, r5, r10 int
	var latency time.Duration
	for _, proj := range manifest.Projects {
		casesPath := proj.CasesPath
		if holdout {
			casesPath = proj.CasesHoldoutPath
		}
		cases, err := eval.LoadCases(casesPath)
		if err != nil {
			return nil, err
		}
		srv, err := evalharness.Start(bin, proj.IndexPath)
		if err != nil {
			return nil, err
		}
		srv.SetSkipIntent(skipIntent)
		for _, c := range cases {
			t0 := time.Now()
			ranked, err := srv.FindContext(c.Query, 10)
			if err != nil {
				srv.Stop()
				return nil, err
			}
			latency += time.Since(t0)
			h1, h3, a5, a10 := ScoreCase(c, ranked)
			n++
			if h1 {
				hit1++
			}
			if h3 {
				hit3++
			}
			if a5 {
				r5++
			}
			if a10 {
				r10++
			}
		}
		srv.Stop()
	}
	if n == 0 {
		return nil, fmt.Errorf("no cases")
	}
	return &GenResult{n, pct(hit1, n), pct(hit3, n), pct(r5, n), pct(r10, n),
		(latency / time.Duration(n)).Milliseconds()}, nil
}

// ScoreCase mirrors cmd/eval: Hit@K is any-of, Recall@K honors min_hits.
func ScoreCase(c eval.QueryCase, ranked []string) (hit1, hit3, r5, r10 bool) {
	exp := make(map[string]bool, len(c.Expected))
	for _, e := range c.Expected {
		exp[e] = true
	}
	minHits := c.MinHits
	if minHits <= 0 {
		minHits = 1
	}
	countIn := func(k int) int {
		if k > len(ranked) {
			k = len(ranked)
		}
		n := 0
		for _, name := range ranked[:k] {
			if exp[name] {
				n++
			}
		}
		return n
	}
	return countIn(1) >= 1, countIn(3) >= 1, countIn(5) >= minHits, countIn(10) >= minHits
}

func runSelf(bin, index string, n int, skipIntent bool) (*SelfResult, error) {
	cases, _, err := selfcases.Sample(index, selfcases.Options{N: n})
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no documented behavioral symbols")
	}
	srv, err := evalharness.Start(bin, index)
	if err != nil {
		return nil, err
	}
	defer srv.Stop()
	srv.SetSkipIntent(skipIntent)

	var hit1, hit5, hit10, miss, seedLoss, hit5Name int
	var latency time.Duration
	for _, c := range cases {
		t0 := time.Now()
		names, err := srv.FindContext(c.Paraphrase(), 10)
		if err != nil {
			return nil, err
		}
		latency += time.Since(t0)
		rank := indexOf(names, c.Qualified)
		if r := indexOfShortName(names, c.Name); r >= 1 && r <= 5 {
			hit5Name++
		}
		switch {
		case rank == 1:
			hit1, hit5, hit10 = hit1+1, hit5+1, hit10+1
		case rank >= 2 && rank <= 5:
			hit5, hit10 = hit5+1, hit10+1
		case rank >= 6:
			hit10++
		default:
			miss++
			// Classify: reachable deeper means ranking lost it, absent means
			// the seed channels never retrieved it. Different levers.
			deep, err := srv.FindContext(c.Paraphrase(), 50)
			if err != nil {
				return nil, err
			}
			if indexOf(deep, c.Qualified) == 0 {
				seedLoss++
			}
		}
	}
	nn := len(cases)
	return &SelfResult{nn, pct(hit1, nn), pct(hit5, nn), pct(hit10, nn), pct(miss, nn),
		pct(hit5Name, nn), pct(seedLoss, nn), (latency / time.Duration(nn)).Milliseconds()}, nil
}

func runNeg(bin, index string) (*NegResult, error) {
	valid, dropped, err := negcases.Verify(index, negcases.Default)
	if err != nil {
		return nil, err
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("every candidate concept exists in this index")
	}
	srv, err := evalharness.Start(bin, index)
	if err != nil {
		return nil, err
	}
	defer srv.Stop()

	falseConf := 0
	for _, c := range valid {
		res, err := srv.Find(c.Query, 5)
		if err != nil {
			return nil, err
		}
		if res.Confidence == "" {
			falseConf++
		}
	}
	return &NegResult{len(valid), len(dropped), falseConf, pct(falseConf, len(valid))}, nil
}

func indexOf(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i + 1
		}
	}
	return 0
}

func pct(a, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(a) / float64(n)
}

// indexOfShortName finds the first result whose last dotted segment matches
// the target's bare name — the interface-declaration / implementation pair.
func indexOfShortName(names []string, short string) int {
	for i, n := range names {
		seg := n
		if k := strings.LastIndex(n, "."); k >= 0 {
			seg = n[k+1:]
		}
		if seg == short {
			return i + 1
		}
	}
	return 0
}
