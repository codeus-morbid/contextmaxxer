package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

type QueryCase struct {
	ID       string   `json:"id"`
	Query    string   `json:"query"`
	Expected []string `json:"expected"`
	// MinHits is the minimum number of `Expected` symbols that must appear in
	// top-K for RecallAt5/RecallAt10 to be true. Default 1 (any-of semantics).
	// Used for multi-hop queries where we want "find 3 of 4 flow stages in top-5".
	MinHits int `json:"min_hits,omitempty"`
	// NotExpected lists symbols that are lexical/semantic traps. If any of these
	// appear in top-5, TrapsHit is incremented. Doesn't affect Recall directly,
	// reported as a separate precision-against-traps metric.
	NotExpected []string `json:"not_expected,omitempty"`
	// Category is informational: paraphrastic|multi-hop|intent-vs-def|negative|cross-language.
	Category string `json:"category,omitempty"`
	// Tags label eval slices (sibling-ambiguous, constructor, layer-conflict, ...).
	// Used for per-slice metric aggregation; a case may carry several tags.
	Tags []string `json:"tags,omitempty"`
}

type ManifestProject struct {
	Name             string `json:"name"`
	IndexPath        string `json:"index_path"`
	CasesPath        string `json:"cases_path"`
	CasesHoldoutPath string `json:"cases_holdout_path"`
}

type Manifest struct {
	Projects []ManifestProject `json:"projects"`
}

type ProjectSummary struct {
	Project    string
	QueryCount int
	Vector     Summary
	Hybrid     Summary
	AvgLatency time.Duration
}

type GlobalSummary struct {
	QueryCount int
	Vector     Summary
	Hybrid     Summary
}

type Result struct {
	QueryID    string
	Mode       string
	Expected   []string
	TopK       []string
	Symbols    []retrieve.ScoredResult
	Hits       []int
	RecallAt5  bool
	RecallAt10 bool
	MRR        float64
	Latency    time.Duration
	Stats      retrieve.Stats
	// HitsAt5/HitsAt10: count of Expected symbols in top-K (for multi-hop scoring).
	HitsAt5  int
	HitsAt10 int
	// MinHits used for this case (0 means default 1 was applied).
	MinHits int
	// TrapsAt5: count of NotExpected symbols that appear in top-5.
	TrapsAt5 int
	// Tags copied from the QueryCase for per-slice aggregation.
	Tags []string
	// TotalTokens is the packed response size (information-per-token metric).
	TotalTokens int
}

type Summary struct {
	HitAt1     float64
	HitAt3     float64
	RecallAt5  float64
	RecallAt10 float64
	NDCGAt5    float64
	MRR        float64
}

func LoadCases(path string) ([]QueryCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load eval cases: %w", err)
	}
	var cases []QueryCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("parse eval cases: %w", err)
	}
	return cases, nil
}

func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("load eval manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse eval manifest: %w", err)
	}
	if len(m.Projects) == 0 {
		return Manifest{}, fmt.Errorf("eval manifest has no projects")
	}
	baseDir := filepath.Dir(path)
	for i := range m.Projects {
		p := &m.Projects[i]
		if strings.TrimSpace(p.Name) == "" {
			return Manifest{}, fmt.Errorf("project[%d] missing name", i)
		}
		if strings.TrimSpace(p.IndexPath) == "" {
			return Manifest{}, fmt.Errorf("project[%s] missing index_path", p.Name)
		}
		if strings.TrimSpace(p.CasesPath) == "" {
			return Manifest{}, fmt.Errorf("project[%s] missing cases_path", p.Name)
		}
		if !filepath.IsAbs(p.IndexPath) {
			p.IndexPath = filepath.Join(baseDir, p.IndexPath)
		}
		if !filepath.IsAbs(p.CasesPath) {
			p.CasesPath = filepath.Join(baseDir, p.CasesPath)
		}
		if p.CasesHoldoutPath != "" && !filepath.IsAbs(p.CasesHoldoutPath) {
			p.CasesHoldoutPath = filepath.Join(baseDir, p.CasesHoldoutPath)
		}
	}
	return m, nil
}

func RunEval(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode) []Result {
	return RunEvalAlphaRerankK(ctx, retriever, cases, mode, 0.7, 0)
}

func RunEvalAlpha(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode, alpha float32) []Result {
	return RunEvalAlphaRerankK(ctx, retriever, cases, mode, alpha, 0)
}

func RunEvalAlphaRerankK(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode, alpha float32, rerankK int) []Result {
	return RunEvalAlphaRerankKAdaptive(ctx, retriever, cases, mode, alpha, rerankK, false)
}

func RunEvalAlphaRerankKAdaptive(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode, alpha float32, rerankK int, adaptiveRerank bool) []Result {
	return RunEvalFullWithAlphaSet(ctx, retriever, cases, mode, alpha, true, rerankK, adaptiveRerank, false, false)
}

func RunEvalFull(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode, alpha float32, rerankK int, adaptiveRerank bool, skipRerank, skipIntent bool) []Result {
	return RunEvalFullWithAlphaSet(ctx, retriever, cases, mode, alpha, true, rerankK, adaptiveRerank, skipRerank, skipIntent)
}

func RunEvalFullDPP(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode, alpha float32, rerankK int, adaptiveRerank bool, skipRerank, skipIntent bool, _ float32, _ bool, _ bool) []Result {
	modeName := "vector-only"
	if mode == retrieve.ModeHybrid {
		modeName = "hybrid"
	}
	results := make([]Result, 0, len(cases))
	for _, c := range cases {
		t0 := time.Now()
		res, err := retriever.Retrieve(ctx, retrieve.Request{
			Query:          c.Query,
			BudgetTokens:   8000,
			SeedK:          20,
			MaxResults:     10,
			RerankK:        rerankK,
			AdaptiveRerank: adaptiveRerank,
			Mode:           mode,
			Alpha:          alpha,
			AlphaSet:       true,
			SkipRerank:     skipRerank,
			SkipIntent:     skipIntent,
		})
		lat := time.Since(t0)
		if err != nil {
			results = append(results, Result{
				QueryID: c.ID,
				Mode:    modeName,
				Latency: lat,
			})
			continue
		}

		topK := make([]string, 0, len(res.Symbols))
		for _, s := range res.Symbols {
			topK = append(topK, s.QualifiedName)
		}

		r := Result{
			QueryID:  c.ID,
			Mode:     modeName,
			Expected: append([]string(nil), c.Expected...),
			TopK:     topK,
			Symbols:  append([]retrieve.ScoredResult(nil), res.Symbols...),
			Latency:  lat,
			Stats:    res.Stats,
		}

		expectedSet := make(map[string]bool, len(c.Expected))
		for _, e := range c.Expected {
			expectedSet[e] = true
		}
		notExpectedSet := make(map[string]bool, len(c.NotExpected))
		for _, e := range c.NotExpected {
			notExpectedSet[e] = true
		}
		minHits := c.MinHits
		if minHits < 1 {
			minHits = 1
		}
		r.MinHits = minHits

		firstHit := 0
		for pos, qn := range topK {
			rank := pos + 1
			if expectedSet[qn] {
				r.Hits = append(r.Hits, rank)
				if firstHit == 0 {
					firstHit = rank
				}
				if rank <= 5 {
					r.HitsAt5++
				}
				if rank <= 10 {
					r.HitsAt10++
				}
			}
			if rank <= 5 && notExpectedSet[qn] {
				r.TrapsAt5++
			}
		}
		// DECISION: Recall@K passes only if at least MinHits expected symbols are in top-K.
		// Default MinHits=1 preserves any-of semantics for legacy single-target queries.
		r.RecallAt5 = r.HitsAt5 >= minHits
		r.RecallAt10 = r.HitsAt10 >= minHits
		if firstHit > 0 {
			r.MRR = 1.0 / float64(firstHit)
		}
		results = append(results, r)
	}
	return results
}

func RunEvalFullWithAlphaSet(ctx context.Context, retriever *retrieve.Retriever, cases []QueryCase, mode retrieve.Mode, alpha float32, alphaSet bool, rerankK int, adaptiveRerank bool, skipRerank, skipIntent bool, opts ...func(*retrieve.Request)) []Result {
	modeName := "vector-only"
	if mode == retrieve.ModeHybrid {
		modeName = "hybrid"
	}
	results := make([]Result, 0, len(cases))
	for _, c := range cases {
		req := retrieve.Request{
			Query:          c.Query,
			BudgetTokens:   8000,
			SeedK:          20,
			MaxResults:     10,
			RerankK:        rerankK,
			AdaptiveRerank: adaptiveRerank,
			Mode:           mode,
			Alpha:          alpha,
			AlphaSet:       alphaSet,
			SkipRerank:     skipRerank,
			SkipIntent:     skipIntent,
		}
		for _, opt := range opts {
			opt(&req)
		}
		t0 := time.Now()
		res, err := retriever.Retrieve(ctx, req)
		lat := time.Since(t0)
		if err != nil {
			results = append(results, Result{
				QueryID: c.ID,
				Mode:    modeName,
				Latency: lat,
			})
			continue
		}

		topK := make([]string, 0, len(res.Symbols))
		for _, s := range res.Symbols {
			topK = append(topK, s.QualifiedName)
		}

		r := Result{
			QueryID:     c.ID,
			Mode:        modeName,
			Expected:    append([]string(nil), c.Expected...),
			TopK:        topK,
			Symbols:     append([]retrieve.ScoredResult(nil), res.Symbols...),
			Latency:     lat,
			Stats:       res.Stats,
			Tags:        append([]string(nil), c.Tags...),
			TotalTokens: res.TotalTokens,
		}

		expectedSet := make(map[string]bool, len(c.Expected))
		for _, e := range c.Expected {
			expectedSet[e] = true
		}
		notExpectedSet := make(map[string]bool, len(c.NotExpected))
		for _, e := range c.NotExpected {
			notExpectedSet[e] = true
		}
		minHits := c.MinHits
		if minHits < 1 {
			minHits = 1
		}
		r.MinHits = minHits

		firstHit := 0
		for pos, qn := range topK {
			rank := pos + 1
			if expectedSet[qn] {
				r.Hits = append(r.Hits, rank)
				if firstHit == 0 {
					firstHit = rank
				}
				if rank <= 5 {
					r.HitsAt5++
				}
				if rank <= 10 {
					r.HitsAt10++
				}
			}
			if rank <= 5 && notExpectedSet[qn] {
				r.TrapsAt5++
			}
		}
		// DECISION: Recall@K passes only if at least MinHits expected symbols are in top-K.
		// Default MinHits=1 preserves any-of semantics for legacy single-target queries.
		r.RecallAt5 = r.HitsAt5 >= minHits
		r.RecallAt10 = r.HitsAt10 >= minHits
		if firstHit > 0 {
			r.MRR = 1.0 / float64(firstHit)
		}
		results = append(results, r)
	}
	return results
}

// SummarizeByTag aggregates metrics per slice tag. A result contributes to
// every tag it carries; untagged results land in the "untagged" bucket.
func SummarizeByTag(results []Result) (map[string]Summary, map[string]int) {
	byTag := make(map[string][]Result)
	for _, r := range results {
		if len(r.Tags) == 0 {
			byTag["untagged"] = append(byTag["untagged"], r)
			continue
		}
		for _, t := range r.Tags {
			byTag[t] = append(byTag[t], r)
		}
	}
	summaries := make(map[string]Summary, len(byTag))
	counts := make(map[string]int, len(byTag))
	for tag, rs := range byTag {
		summaries[tag] = SummarizeDetailed(rs)
		counts[tag] = len(rs)
	}
	return summaries, counts
}

type DiagnosticCandidate struct {
	Rank          int                      `json:"rank"`
	File          string                   `json:"file"`
	QualifiedName string                   `json:"qualified_name"`
	Kind          string                   `json:"kind"`
	Score         float32                  `json:"score"`
	Why           string                   `json:"why"`
	Features      retrieve.RankingFeatures `json:"features"`
	Hit           bool                     `json:"hit"`
}

type DiagnosticRow struct {
	QueryID        string                `json:"query_id"`
	Mode           string                `json:"mode"`
	Expected       []string              `json:"expected"`
	EffectiveAlpha float32               `json:"effective_alpha"`
	Candidates     []DiagnosticCandidate `json:"candidates"`
}

func DumpRankingDiagnostics(path string, results []Result) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create ranking diagnostics dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create ranking diagnostics: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, result := range results {
		expected := make(map[string]struct{}, len(result.Expected))
		for _, name := range result.Expected {
			expected[name] = struct{}{}
		}
		row := DiagnosticRow{
			QueryID:        result.QueryID,
			Mode:           result.Mode,
			Expected:       result.Expected,
			EffectiveAlpha: result.Stats.EffectiveAlpha,
		}
		for i, sym := range result.Symbols {
			_, hit := expected[sym.QualifiedName]
			row.Candidates = append(row.Candidates, DiagnosticCandidate{
				Rank:          i + 1,
				File:          sym.File,
				QualifiedName: sym.QualifiedName,
				Kind:          sym.Kind,
				Score:         sym.Score,
				Why:           sym.Why,
				Features:      sym.Features,
				Hit:           hit,
			})
		}
		if err := enc.Encode(row); err != nil {
			return fmt.Errorf("write ranking diagnostics: %w", err)
		}
	}
	return nil
}

func Summarize(results []Result) (recallAt5, recallAt10, mrr float64) {
	s := SummarizeDetailed(results)
	return s.RecallAt5, s.RecallAt10, s.MRR
}

func ndcgAt5(topK []string, expected []string) float64 {
	expectedSet := make(map[string]bool, len(expected))
	for _, e := range expected {
		expectedSet[e] = true
	}
	var dcg float64
	for i := 0; i < 5 && i < len(topK); i++ {
		if expectedSet[topK[i]] {
			dcg += 1.0 / math.Log2(float64(i+2))
		}
	}
	var idcg float64
	idealLen := len(expected)
	if idealLen > 5 {
		idealLen = 5
	}
	for i := 0; i < idealLen; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func SummarizeDetailed(results []Result) Summary {
	if len(results) == 0 {
		return Summary{}
	}
	var s Summary
	for _, r := range results {
		if len(r.Hits) > 0 && r.Hits[0] == 1 {
			s.HitAt1++
		}
		for _, hit := range r.Hits {
			if hit <= 3 {
				s.HitAt3++
				break
			}
		}
		if r.RecallAt5 {
			s.RecallAt5++
		}
		if r.RecallAt10 {
			s.RecallAt10++
		}
		s.NDCGAt5 += ndcgAt5(r.TopK, r.Expected)
		s.MRR += r.MRR
	}
	n := float64(len(results))
	s.HitAt1 /= n
	s.HitAt3 /= n
	s.RecallAt5 /= n
	s.RecallAt10 /= n
	s.NDCGAt5 /= n
	s.MRR /= n
	return s
}

func BuildProjectSummary(project string, vector, hybrid []Result) ProjectSummary {
	var total time.Duration
	for _, r := range hybrid {
		total += r.Latency
	}
	avg := time.Duration(0)
	if len(hybrid) > 0 {
		avg = total / time.Duration(len(hybrid))
	}
	return ProjectSummary{
		Project:    project,
		QueryCount: len(hybrid),
		Vector:     SummarizeDetailed(vector),
		Hybrid:     SummarizeDetailed(hybrid),
		AvgLatency: avg,
	}
}

func SummarizeGlobal(projects []ProjectSummary) GlobalSummary {
	var g GlobalSummary
	if len(projects) == 0 {
		return g
	}
	for _, p := range projects {
		n := float64(p.QueryCount)
		g.QueryCount += p.QueryCount
		g.Vector.HitAt1 += p.Vector.HitAt1 * n
		g.Vector.HitAt3 += p.Vector.HitAt3 * n
		g.Vector.RecallAt5 += p.Vector.RecallAt5 * n
		g.Vector.RecallAt10 += p.Vector.RecallAt10 * n
		g.Vector.NDCGAt5 += p.Vector.NDCGAt5 * n
		g.Vector.MRR += p.Vector.MRR * n
		g.Hybrid.HitAt1 += p.Hybrid.HitAt1 * n
		g.Hybrid.HitAt3 += p.Hybrid.HitAt3 * n
		g.Hybrid.RecallAt5 += p.Hybrid.RecallAt5 * n
		g.Hybrid.RecallAt10 += p.Hybrid.RecallAt10 * n
		g.Hybrid.NDCGAt5 += p.Hybrid.NDCGAt5 * n
		g.Hybrid.MRR += p.Hybrid.MRR * n
	}
	if g.QueryCount == 0 {
		return g
	}
	d := float64(g.QueryCount)
	g.Vector.HitAt1 /= d
	g.Vector.HitAt3 /= d
	g.Vector.RecallAt5 /= d
	g.Vector.RecallAt10 /= d
	g.Vector.NDCGAt5 /= d
	g.Vector.MRR /= d
	g.Hybrid.HitAt1 /= d
	g.Hybrid.HitAt3 /= d
	g.Hybrid.RecallAt5 /= d
	g.Hybrid.RecallAt10 /= d
	g.Hybrid.NDCGAt5 /= d
	g.Hybrid.MRR /= d
	return g
}
