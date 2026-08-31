package main

import (
	"math"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
)

func close(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 0.001 {
		t.Fatalf("%s: got %.4f want %.4f", what, got, want)
	}
}

// The numbers below were worked out by hand before the code ran: gold is a.go
// lines 10-12 and b.go lines 1-2 (five lines over two files); the response
// returns a.go showing 10-11 and c.go showing 1-5 (seven lines over two files,
// two of them gold).
func TestScore_WorkedExample(t *testing.T) {
	inst := instance{
		GoldFiles: []string{"a.go", "b.go"},
		GoldRegions: []region{
			{Path: "a.go", Start: 10, End: 12},
			{Path: "b.go", Start: 1, End: 2},
		},
	}
	res := evalharness.Result{
		Files:   []string{"a.go", "c.go"},
		Visible: []string{"10-11", "1-5"},
		Lines:   []string{"1-100", "1-100"},
	}

	m := score(inst, res)
	close(t, m.hitFile, 1, "hitFile")
	close(t, m.fileRecall, 0.5, "fileRecall")
	close(t, m.ndcg, 1/(1+1/math.Log2(3)), "ndcg")
	close(t, m.lineRecall, 2.0/5.0, "lineRecall")
	close(t, m.efficiency, 2.0/7.0, "efficiency")
	if m.fuh != 1 {
		t.Fatalf("first useful hit: got %d want 1", m.fuh)
	}
}

// A response returns one entry per SYMBOL, so several entries commonly share a
// file. Worked out by hand before the fix: gold is a.go alone, the response is
// [a.go, a.go, b.go]. Only the first a.go is relevant, so DCG = 1/log2(2) = 1
// and IDCG over one gold file = 1, giving exactly 1. Crediting the repeat gave
// 1 + 1/log2(3) = 1.631 over the same IDCG — an nDCG above 1, which is not a
// score at all.
func TestScore_RepeatedGoldFileCannotPushNDCGAboveOne(t *testing.T) {
	inst := instance{GoldFiles: []string{"a.go"}}
	res := evalharness.Result{Files: []string{"a.go", "a.go", "b.go"}}

	m := score(inst, res)
	close(t, m.ndcg, 1, "ndcg")
	close(t, m.fileRecall, 1, "one unique gold file was returned")
}

func TestScore_CompleteMissScoresZero(t *testing.T) {
	// The metrics have to be able to say "nothing useful", or a run that found
	// nothing would still look like partial credit.
	inst := instance{
		GoldFiles:   []string{"a.go"},
		GoldRegions: []region{{Path: "a.go", Start: 1, End: 10}},
	}
	res := evalharness.Result{Files: []string{"z.go"}, Visible: []string{"1-50"}}

	m := score(inst, res)
	close(t, m.hitFile, 0, "hitFile")
	close(t, m.fileRecall, 0, "fileRecall")
	close(t, m.ndcg, 0, "ndcg")
	close(t, m.lineRecall, 0, "lineRecall")
	close(t, m.efficiency, 0, "efficiency")
	if m.fuh != 0 {
		t.Fatalf("no gold file was returned, so there is no first useful hit: got %d", m.fuh)
	}
}

func TestScore_PerfectAnswerScoresOne(t *testing.T) {
	inst := instance{
		GoldFiles:   []string{"a.go"},
		GoldRegions: []region{{Path: "a.go", Start: 5, End: 9}},
	}
	res := evalharness.Result{Files: []string{"a.go"}, Visible: []string{"5-9"}}

	m := score(inst, res)
	close(t, m.hitFile, 1, "hitFile")
	close(t, m.fileRecall, 1, "fileRecall")
	close(t, m.ndcg, 1, "ndcg")
	close(t, m.lineRecall, 1, "lineRecall")
	close(t, m.efficiency, 1, "efficiency")
}

func TestScore_EfficiencyPunishesOverReturning(t *testing.T) {
	// Returning the whole file when five lines were wanted finds everything and
	// costs the agent the rest. Recall must stay 1 while efficiency collapses —
	// that split is the reason this benchmark is worth running.
	inst := instance{
		GoldFiles:   []string{"a.go"},
		GoldRegions: []region{{Path: "a.go", Start: 1, End: 5}},
	}
	res := evalharness.Result{Files: []string{"a.go"}, Visible: []string{"1-500"}}

	m := score(inst, res)
	close(t, m.lineRecall, 1, "lineRecall")
	close(t, m.efficiency, 5.0/500.0, "efficiency")
}

func TestScore_UsesVisibleSpanNotTheFullSymbol(t *testing.T) {
	// A trimmed response must be charged for what it actually sent. Scoring the
	// symbol's full extent would count lines the agent never saw as both found
	// and paid for.
	inst := instance{
		GoldFiles:   []string{"a.go"},
		GoldRegions: []region{{Path: "a.go", Start: 1, End: 100}},
	}
	trimmed := evalharness.Result{
		Files:   []string{"a.go"},
		Lines:   []string{"1-100"},
		Visible: []string{"1-10"},
	}
	m := score(inst, trimmed)
	close(t, m.lineRecall, 10.0/100.0, "lineRecall must reflect the ten lines actually shown")
	close(t, m.efficiency, 1, "every shown line was gold")
}

func TestScore_MultipleVisibleWindows(t *testing.T) {
	// Evidence selection returns several windows of one symbol as "12-14,40-42".
	inst := instance{
		GoldFiles:   []string{"a.go"},
		GoldRegions: []region{{Path: "a.go", Start: 12, End: 14}, {Path: "a.go", Start: 40, End: 42}},
	}
	res := evalharness.Result{Files: []string{"a.go"}, Visible: []string{"12-14,40-42"}}

	m := score(inst, res)
	close(t, m.lineRecall, 1, "both windows count")
	close(t, m.efficiency, 1, "and neither is waste")
}

func TestNormPath_MakesWindowsAndBenchmarkPathsComparable(t *testing.T) {
	if got := normPath(`xarray\core\merge.py`); got != "xarray/core/merge.py" {
		t.Fatalf("backslashes not normalised: %s", got)
	}
	if got := normPath("./a/b.go"); got != "a/b.go" {
		t.Fatalf("leading ./ not trimmed: %s", got)
	}
}

func TestParseSpan(t *testing.T) {
	for _, tc := range []struct {
		in         string
		start, end int
		ok         bool
	}{
		{"10-20", 10, 20, true},
		{" 3-3 ", 3, 3, true},
		{"20-10", 0, 0, false},
		{"abc", 0, 0, false},
		{"", 0, 0, false},
	} {
		start, end, ok := parseSpan(tc.in)
		if ok != tc.ok || start != tc.start || end != tc.end {
			t.Fatalf("parseSpan(%q) = %d,%d,%v want %d,%d,%v", tc.in, start, end, ok, tc.start, tc.end, tc.ok)
		}
	}
}
