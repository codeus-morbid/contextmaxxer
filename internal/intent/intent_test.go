package intent

import (
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/stretchr/testify/require"
)

func TestRankerPromotesConstructorIntentOverStoreMethods(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/store/sqlite/sqlite.go", QualifiedName: "sqlite.Store.ListFiles", Kind: "method", Score: 0.69},
		{File: "internal/store/sqlite/sqlite.go", QualifiedName: "sqlite.Store.ListAllEdges", Kind: "method", Score: 0.62},
		{File: "internal/store/sqlite/sqlite.go", QualifiedName: "sqlite.New", Kind: "function", Score: 0.76, Features: retrieve.RankingFeatures{IsConstructor: 1}},
	}

	got := ranker.Rank("how is sqlite store opened", items)

	require.Equal(t, "sqlite.New", got[0].QualifiedName)
}

func TestRankerPromotesLanguageSpecificExtractor(t *testing.T) {
	profile := Analyze("extract Go symbols from syntax tree")
	require.Equal(t, "go", profile.Language)

	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/index/languages/python.go", QualifiedName: "languages.pyExtractor.Symbols", Kind: "method", Score: 0.97},
		{File: "internal/index/languages/typescript.go", QualifiedName: "languages.tsExtractor.Symbols", Kind: "method", Score: 0.86},
		{File: "internal/index/languages/golang.go", QualifiedName: "languages.goExtractor.Symbols", Kind: "method", Score: 0.85},
	}

	got := ranker.Rank("extract Go symbols from syntax tree", items)

	require.Equal(t, "languages.goExtractor.Symbols", got[0].QualifiedName)
}

func TestRankerPromotesImplementationOverPassiveTypeForActionQuery(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/retrieve/pagerank.go", QualifiedName: "retrieve.Graph", Kind: "class", Score: 0.34},
		{File: "internal/retrieve/pagerank.go", QualifiedName: "retrieve.BuildGraph", Kind: "function", Score: 0.84},
	}

	got := ranker.Rank("build the symbol call graph", items)

	require.Equal(t, "retrieve.BuildGraph", got[0].QualifiedName)
}

func TestRankerPromotesExactIdentifierTokenCoverage(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/store/sqlite/sqlite.go", QualifiedName: "sqlite.Store.SearchByVector", Kind: "method", Score: 0.79},
		{
			File:          "internal/store/sqlite/sqlite.go",
			QualifiedName: "sqlite.Store.SearchByVectorScored",
			Kind:          "method",
			Score:         0.78,
			Features: retrieve.RankingFeatures{
				ShortNameOverlap: 0.50,
				NameOverlap:      0.50,
				SignatureOverlap: 0.50,
				PathOverlap:      0.25,
				KindMethod:       1,
			},
		},
	}

	got := ranker.Rank("search symbols by vector similarity", items)

	require.Equal(t, "sqlite.Store.SearchByVectorScored", got[0].QualifiedName)
}

func TestRankerWindowCoversRerankPool(t *testing.T) {
	// The intent window is 15 (the rerank pool size): a strong signal can
	// rescue rank 11-15, but rank 16+ stays where the fused ranking put it.
	ranker := NewRanker()
	var items []retrieve.ScoredResult
	for i := 0; i < 16; i++ {
		items = append(items, retrieve.ScoredResult{
			QualifiedName: "pkg.Candidate",
			Kind:          "function",
			Score:         float32(100 - i),
		})
	}
	items[15].QualifiedName = "sqlite.New"
	items[15].Features.IsConstructor = 1

	got := ranker.Rank("how is sqlite store opened", items)

	require.Equal(t, "sqlite.New", got[15].QualifiedName, "rank 16 is outside the intent window")
}

func TestRankerRescuesConstructorFromRankEleven(t *testing.T) {
	// Regression case: "construct the employee
	// history service" left NewEmployeeHistoryService at rank 11 — outside
	// the old window of 10 — while the class it constructs sat at rank 1.
	ranker := NewRanker()
	items := []retrieve.ScoredResult{{
		File: "internal/service/employee_history_service.go", QualifiedName: "service.EmployeeHistoryService", Kind: "class", Score: 0.95,
	}}
	for i := 0; i < 9; i++ {
		items = append(items, retrieve.ScoredResult{
			File: "internal/service/etl_service.go", QualifiedName: "service.ETLService.processEmployeeHistory", Kind: "method", Score: float32(0.94) - float32(i)*0.002,
		})
	}
	items = append(items, retrieve.ScoredResult{
		File: "internal/service/employee_history_service.go", QualifiedName: "service.NewEmployeeHistoryService", Kind: "function", Score: 0.93,
		Features: retrieve.RankingFeatures{IsConstructor: 1},
	})

	got := ranker.Rank("construct the employee history service", items)

	require.LessOrEqual(t, indexOf(t, got, "service.NewEmployeeHistoryService"), 2)
}

func TestRankerDemotesDataClassesForVerbLeadQuery(t *testing.T) {
	// Regression case: plain data/model classes outranked
	// the method that does the work for an imperative query.
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/models/ai_analysis_translation.go", QualifiedName: "models.MatchAIAnalysisTranslation", Kind: "class", Score: 0.92},
		{File: "internal/models/match_ai_analysis.go", QualifiedName: "models.MatchAIAnalysis", Kind: "class", Score: 0.91},
		{File: "internal/parser/match_ai_repair.go", QualifiedName: "parser.MatchAIAnalysisParser.attemptRepairAnalysis", Kind: "method", Score: 0.90},
	}

	got := ranker.Rank("retry ai analysis generation when the model returned broken json", items)

	require.Equal(t, "parser.MatchAIAnalysisParser.attemptRepairAnalysis", got[0].QualifiedName)
}

func TestVerbLeadDetectionIsPositional(t *testing.T) {
	// Counter-guard: a query that expects the API class
	// leading with a noun must NOT trigger the implementation preference even
	// though a verb ("calls") appears later. Leading adverbs are skipped.
	require.False(t, Analyze("browser wrapper that calls backend endpoints with the auth token").PreferImplementation)
	require.True(t, Analyze("retry ai analysis generation when the model returned broken json").PreferImplementation)
	require.True(t, Analyze("automatically buy matched snipe targets under fair value").PreferImplementation)
	require.True(t, Analyze("rank the strongest employees by a weighted score").PreferImplementation)
}

func TestRankerDemotesTestInfraUnlessQueryAsks(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "pkg/testutils/settle_helpers.go", QualifiedName: "testutils.SettleBets", Kind: "function", Score: 0.91},
		{File: "internal/service/settlement.go", QualifiedName: "service.SettleBets", Kind: "function", Score: 0.90},
	}

	got := ranker.Rank("settle bets for a finished series", items)
	require.Equal(t, "service.SettleBets", got[0].QualifiedName)

	got = ranker.Rank("test helper that settles bets", items)
	require.Equal(t, "testutils.SettleBets", got[0].QualifiedName)
}

func indexOf(t *testing.T, items []retrieve.ScoredResult, qn string) int {
	t.Helper()
	for i, it := range items {
		if it.QualifiedName == qn {
			return i
		}
	}
	t.Fatalf("%s not found in results", qn)
	return -1
}

func TestRankerPromotesStartWhenQueryAsksStart(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/scheduler/scheduler.go", QualifiedName: "scheduler.Scheduler.kickManualRefreshWorker", Kind: "method", Score: 1.00},
		{File: "internal/scheduler/scheduler.go", QualifiedName: "scheduler.Scheduler.Start", Kind: "method", Score: 0.96},
	}

	got := ranker.Rank("which method starts scheduler loops", items)

	require.Equal(t, "scheduler.Scheduler.Start", got[0].QualifiedName)
}

func TestRankerPenalizesAdminWhenQueryNotAdmin(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{File: "internal/api/admin_matches.go", QualifiedName: "api.APIHandler.AdminRefreshUpcomingMatches", Kind: "method", Score: 1.00},
		{File: "internal/api/handlers.go", QualifiedName: "api.APIHandler.GetMatches", Kind: "method", Score: 0.98},
	}

	got := ranker.Rank("api handler that returns matches list", items)

	require.Equal(t, "api.APIHandler.GetMatches", got[0].QualifiedName)
}

func TestRankerUsesFieldFeaturesForSiblingDisambiguation(t *testing.T) {
	ranker := NewRanker()
	items := []retrieve.ScoredResult{
		{
			File:          "internal/handler/employee_handler.go",
			QualifiedName: "handler.EmployeeHandler.GetEmployeeByID",
			Kind:          "method",
			Score:         0.985,
			Features: retrieve.RankingFeatures{
				ShortNameOverlap: 0.33,
				NameOverlap:      0.33,
				SignatureOverlap: 0.33,
				PathOverlap:      0.33,
				KindMethod:       1,
			},
		},
		{
			File:          "internal/handler/employee_handler.go",
			QualifiedName: "handler.EmployeeHandler.GetEmployees",
			Kind:          "method",
			Score:         0.98,
			Features: retrieve.RankingFeatures{
				ShortNameOverlap: 0.50,
				NameOverlap:      0.50,
				SignatureOverlap: 0.50,
				PathOverlap:      0.33,
				KindMethod:       1,
			},
		},
	}

	got := ranker.Rank("http handler returns employee list", items)

	require.Equal(t, "handler.EmployeeHandler.GetEmployees", got[0].QualifiedName)
}
