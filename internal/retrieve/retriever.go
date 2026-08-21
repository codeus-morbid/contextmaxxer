package retrieve

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type OutputMode int

const (
	OutputModeAnswer  OutputMode = iota
	OutputModeMinimal OutputMode = 1
	OutputModeExplore OutputMode = 2
)

func (m OutputMode) String() string {
	switch m {
	case OutputModeMinimal:
		return "minimal"
	case OutputModeExplore:
		return "explore"
	default:
		return "answer"
	}
}

func ParseOutputMode(s string) (OutputMode, error) {
	switch s {
	case "minimal":
		return OutputModeMinimal, nil
	case "explore":
		return OutputModeExplore, nil
	case "answer", "":
		return OutputModeAnswer, nil
	default:
		return OutputModeAnswer, fmt.Errorf("unknown output mode %q: use answer, minimal, or explore", s)
	}
}

type SymbolRef struct {
	QualifiedName string
	File          string
	Lines         string
	Kind          string
	// CallLine is the file line of the call site: for a callee, where the result
	// symbol calls it; for a caller, where that caller calls the result symbol.
	// 0 when not located. Lets the agent follow a call chain without opening files.
	CallLine int
	// PathStatus is "static_unverified" for call-graph edges. Extractors prove
	// that the call exists in source, not that its surrounding branch executes.
	PathStatus string
	// CallSite is a bounded, line-numbered source window around CallLine. It is
	// populated for top-result graph refs so agents can inspect nearby branch or
	// dispatch conditions without paying for every related symbol body.
	CallSite string
}

type ScoredResult struct {
	SymbolID      int64
	File          string
	QualifiedName string
	Kind          string
	Signature     string
	Docstring     string
	StartLine     int
	EndLine       int
	Score         float32
	Body          string
	Why           string
	AlsoVia       []string
	Features      RankingFeatures

	// Relevance is the cross-encoder's own score (sigmoid, 0..1) for this
	// query/document pair — set only when the reranker ran. Score, by
	// contrast, accumulates ranking bonuses (intent priors add up to ~+4),
	// which makes it useless as an absolute "is this actually a match"
	// signal. 0 means "no cross-encoder verdict" (rerank skipped or failed).
	Relevance float32

	// Detail marks how much of the symbol the packed Body carries:
	// "full" (complete indexed body), "excerpt" (a query-relevant source
	// window), or "compact" (signature + doc line).
	Detail string

	// BodyStartLine is the real file line number of the first line of Body. It
	// equals StartLine unless an evidence-span trim moved the body window down,
	// in which case it lets the output number the trimmed lines correctly.
	BodyStartLine int
	// BodyEndLine is the real file line number of the last visible Body line.
	// It differs from EndLine when Detail is "excerpt".
	BodyEndLine int

	Confidence string
	Callers    []SymbolRef
	Callees    []SymbolRef

	Visibility     string
	Tests          []SymbolRef
	Siblings       []SymbolRef
	FlowContext    []FlowRef
	CompanionFiles []string
}

type RankingFeatures struct {
	VectorSeed       float32
	FTSSeed          float32
	PPR              float32
	EffectiveAlpha   float32
	SeedScore        float32
	PPRScore         float32
	SeedRank         int
	PPRRank          int
	ShortNameOverlap float32
	NameOverlap      float32
	PathOverlap      float32
	KindOverlap      float32
	SignatureOverlap float32
	BodyOverlap      float32
	IsConstructor    float32
	KindFunction     float32
	KindMethod       float32
	HubPenalty       float32
}

// AsMap flattens the feature vector for logging. It is the single source of
// truth for which ranking signals reach the feedback log (and thus a future
// learned ranker) — add a field here when you add one to the pipeline.
func (f RankingFeatures) AsMap() map[string]float32 {
	return map[string]float32{
		"vector_seed":        f.VectorSeed,
		"fts_seed":           f.FTSSeed,
		"ppr":                f.PPR,
		"effective_alpha":    f.EffectiveAlpha,
		"seed_score":         f.SeedScore,
		"ppr_score":          f.PPRScore,
		"seed_rank":          float32(f.SeedRank),
		"ppr_rank":           float32(f.PPRRank),
		"short_name_overlap": f.ShortNameOverlap,
		"name_overlap":       f.NameOverlap,
		"path_overlap":       f.PathOverlap,
		"kind_overlap":       f.KindOverlap,
		"signature_overlap":  f.SignatureOverlap,
		"body_overlap":       f.BodyOverlap,
		"is_constructor":     f.IsConstructor,
		"kind_function":      f.KindFunction,
		"kind_method":        f.KindMethod,
		"hub_penalty":        f.HubPenalty,
	}
}

type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type Reranker interface {
	Rerank(ctx context.Context, query string, candidates []ScoredResult) ([]ScoredResult, error)
}

// TextScorer is an optional Reranker capability: score free-form texts against
// the query. Evidence-window selection uses it when the reranker offers it,
// because a cross-encoder reads the query and the window together, while the
// bi-encoder can only compare two vectors built in ignorance of each other.
type TextScorer interface {
	ScoreTexts(ctx context.Context, query string, docs []string) ([]float32, error)
}

type Ranker interface {
	Rank(query string, candidates []ScoredResult) []ScoredResult
}

// IntentRanker is a marker interface implemented by the symbolic intent ranker.
type IntentRanker interface {
	Ranker
	IsIntentRanker()
}

type Store interface {
	SearchByVectorScored(ctx context.Context, vec []float32, k int) ([]store.ScoredSymbol, error)
	SearchByText(ctx context.Context, query string, k int) ([]store.ScoredSymbol, error)
	SearchByBodyText(ctx context.Context, query string, k int) ([]store.ScoredSymbol, error)
	SearchByChunkVector(ctx context.Context, vec []float32, k int) ([]store.ScoredSymbol, error)
	GetSymbolsByIDs(ctx context.Context, ids []int64) ([]store.Symbol, error)
	GetSymbolBody(ctx context.Context, symbolID int64) (store.SymbolBody, error)
	GetFilesByIDs(ctx context.Context, ids []int64) (map[int64]string, error)
	ListAllSymbolIDs(ctx context.Context) ([]int64, error)
	ListSymbolMeta(ctx context.Context) ([]store.Symbol, error)
	ListAllEdges(ctx context.Context) ([]store.Edge, error)
	GetEmbeddingsByIDs(ctx context.Context, ids []int64) (map[int64][]float32, error)
	GetCallerEdges(ctx context.Context, dstIDs []int64, limitPerSymbol int) (map[int64][]int64, error)
	GetCalleeEdges(ctx context.Context, srcIDs []int64, limitPerSymbol int) (map[int64][]int64, error)
}

type Mode int

const (
	ModeHybrid     Mode = iota
	ModeVectorOnly Mode = 1
)

type Retriever struct {
	store    Store
	embedder Embedder
	reranker Reranker
	ranker   Ranker
	// escalator is an optional stronger (slower) reranker applied only when
	// the primary ranking looks ambiguous (low retrieval health).
	escalator Reranker
	log       *slog.Logger
	// graphMemo caches query-independent derived views (lookup maps,
	// adjacency, test index) of the store's symbol/edge caches; freshness is
	// checked by slice identity in getGraphMemo.
	memoMu    sync.Mutex
	graphMemo *graphMemoData
}

// SetEscalator installs a stronger fallback reranker used only for
// low-confidence queries. Nil disables escalation.
func (r *Retriever) SetEscalator(esc Reranker) {
	r.escalator = esc
}

func NewRetriever(s Store, e Embedder, log *slog.Logger) *Retriever {
	return &Retriever{store: s, embedder: e, log: log}
}

func NewRetrieverWithReranker(s Store, e Embedder, reranker Reranker, log *slog.Logger) *Retriever {
	return &Retriever{store: s, embedder: e, reranker: reranker, log: log}
}

func NewRetrieverWithRankers(s Store, e Embedder, reranker Reranker, ranker Ranker, log *slog.Logger) *Retriever {
	return &Retriever{store: s, embedder: e, reranker: reranker, ranker: ranker, log: log}
}

type Request struct {
	Query          string
	BudgetTokens   int
	SeedK          int
	MaxResults     int
	RerankK        int
	AdaptiveRerank bool
	// LazyRerank inverts the reranking default: the cross-encoder runs only
	// when the fused ranking is ambiguous (small gap / several near-ties),
	// instead of on every query.
	LazyRerank bool
	// FullBodyResults caps how many top results keep their full body in the
	// packed response (0 = default 3, negative = all results keep bodies).
	FullBodyResults int
	// PreserveFullBodies disables query-relevant evidence trimming. Serving
	// paths set it only for an explicit full_bodies compatibility override;
	// normal navigation uses excerpts and expand_context for exact hydration.
	PreserveFullBodies bool
	Mode               Mode
	OutputMode         OutputMode
	Alpha              float32
	AlphaSet           bool
	IncludeTrivial     bool
	SkipRerank         bool
	SkipIntent         bool
}

type Stats struct {
	SeedCount         int
	GraphNodes        int
	GraphEdges        int
	PPRIterations     int
	EmbedDuration     time.Duration
	SeedDuration      time.Duration
	GraphDuration     time.Duration
	PPRDuration       time.Duration
	RerankDuration    time.Duration
	IntentDuration    time.Duration
	EscalateDuration  time.Duration
	Escalated         bool
	RerankLazySkipped bool
	PackDuration      time.Duration
	EvidenceDuration  time.Duration
	// GraphCtxDuration covers the callers/callees/tests/siblings SQL
	// enrichment of the selected results (not the PPR graph itself).
	GraphCtxDuration time.Duration
	Total            time.Duration
	EffectiveAlpha   float32
}

type StructureView struct {
	Summary     string
	PackageView map[string][]string
}

type NextStepsHints struct {
	IfTopCorrect string
	IfUnsure     string
	ToExplore    []string
}

type FlowRef struct {
	QualifiedName string
	File          string
	Lines         string
	Kind          string
	Distance      int
	Via           string
}

type RetrievalHealth struct {
	CandidatesSeen int
	TopScoreGap    float32
	TiedCandidates int
	Confidence     string
	Suggestion     string
}

type Result struct {
	Symbols         []ScoredResult
	TotalTokens     int
	Stats           Stats
	Structure       *StructureView
	NextSteps       *NextStepsHints
	RetrievalHealth *RetrievalHealth
	// ExpansionSymbols is an internal snapshot of the selected ranked symbols
	// before tiering and evidence trimming. MCP caches it by request_id so
	// expand_context can hydrate an already-found symbol without rerunning
	// embedding, graph ranking, or reranking.
	ExpansionSymbols []ScoredResult
}

func (r *Retriever) Retrieve(ctx context.Context, req Request) (Result, error) {
	return runPipeline(ctx, r, req)
}

func (r *Retriever) GetSymbolBody(ctx context.Context, symbolID int64) (store.SymbolBody, error) {
	return r.store.GetSymbolBody(ctx, symbolID)
}
