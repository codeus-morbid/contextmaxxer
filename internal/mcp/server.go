package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
	"github.com/codeus-morbid/contextmaxxer/internal/releasecfg"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
)

type Server struct {
	retriever *retrieve.Retriever
	feedback  *feedback.Recorder
	options   Options
	log       *slog.Logger
	// calls counts find_context invocations in this server session; used to
	// auto-compact output defaults on long multi-call sessions.
	calls atomic.Int64
	// expansionCache keeps this MCP process's ranked symbol identities. It lets
	// expand_context hydrate any prior result without a second semantic search;
	// bodies and ranking evidence are deliberately not retained here.
	expansionMu       sync.Mutex
	expansionCache    map[string][]expansionTarget
	continuations     map[string]continuationState
	continuationOrder []string
}

// sessionCompactAfter is the find_context call count past which the default
// full-body count drops to 1. DECISION(2026-07): a 70-stage audit A/B showed
// the default payload (~1-1.5K tokens/call) COMPOUNDING over 68 sequential
// calls into 3x the grep arm's total tokens — the rich default is right for
// the first few calls and wrong for marathon sessions. REVISIT IF: long-session A/B shows reads
// creeping back up (the graph context, which prevents them, stays intact).
const sessionCompactAfter = 4

const continuationCacheLimit = 128
const expansionPageBytes = 48 * 1024

const findContextToolDescription = `Semantic code search for this repository. Returns ranked symbols with line-numbered excerpts, signatures, and caller/callee graph context.
Reach for this on a LARGE codebase you do not already know your way around, especially when following a call chain ("what actually runs when X happens") or when you cannot name what you are looking for and have to describe it. Every result carries callers and callees with the exact call-site line, so a hop costs one call whatever the repo's size, while a text search pays for the size of the haystack. On a small or familiar codebase, or for a symbol whose name you already know, use grep instead — then paste the hit back here as the query, verbatim: a query of the form path/to/file.go:142 (a whole grep output line works too) is answered by index lookup, returning the symbol that encloses that line together with its callers and callees. grep tells you where a name occurs; this tells you what reaches it, which no text search can. That form runs no embedding or reranking, so it is the cheapest call this tool serves.
Call edges are static candidates, not proof that a runtime branch executes: verify the branch, feature flag, protocol, or dispatch discriminator before claiming a runtime path. Graph refs carry path_status=static_unverified and a call_line. Graph context and callsite evidence are carried by the top results, where a call chain is worth following; compact tail entries are candidates only. Cite file:line directly from results.
An excerpt reports how much of the symbol it shows ("26 of 57 lines"). When a question needs every branch of a function — what disables a path, which cases are handled — a window is the wrong shape: expand it rather than answering from the part you were shown.
Modes: 'answer' (default — top matches + graph + confidence), 'minimal' (bodies only, ~50% tokens), 'explore' (+ package overview, for an unfamiliar codebase).
Query shape is the single biggest thing under your control: one sentence naming the mechanism, in the code's vocabulary. Pasting an issue report or a stack trace costs 17% precision against its title alone, measured over 493 tasks — more than any part of this pipeline is worth.
The defaults are calibrated for agent navigation. During normal exploration omit tuning knobs. If an excerpt omits required code, call expand_context with this request_id and the relevant rank; it hydrates the exact indexed body without rerunning semantic search. If it returns status:more, call continue_context with next_cursor until status:complete; do not replace continuation with grep or file reads.
After acting on results, call record_feedback once with this call's request_id and the names you used.`

type symbolRefOut struct {
	Name       string `json:"name"`
	File       string `json:"file"`
	Lines      string `json:"lines"`
	Kind       string `json:"kind"`
	CallLine   int    `json:"call_line,omitempty"`
	PathStatus string `json:"path_status,omitempty"`
	CallSite   string `json:"callsite_evidence,omitempty"`
}

func symbolRefOutputs(refs []retrieve.SymbolRef) []symbolRefOut {
	if len(refs) == 0 {
		return nil
	}
	out := make([]symbolRefOut, len(refs))
	for i, r := range refs {
		out[i] = symbolRefOut{
			Name: r.QualifiedName, File: r.File, Lines: r.Lines, Kind: r.Kind,
			CallLine: r.CallLine, PathStatus: r.PathStatus, CallSite: r.CallSite,
		}
	}
	return out
}

type Options struct {
	AdaptiveRerank bool
	// LazyRerank runs the cross-encoder only when the fused ranking is
	// ambiguous, keeping confident queries on the fast (~100ms) path.
	LazyRerank bool
}

type expansionTarget struct {
	SymbolID      int64
	File          string
	QualifiedName string
	Kind          string
	StartLine     int
	EndLine       int
}

type continuationState struct {
	RequestID string
	Rank      int
	Symbol    retrieve.ScoredResult
	Body      string
	SHA256    string
	Offset    int
}

type expansionPage struct {
	RequestID   string
	Rank        int
	Symbol      retrieve.ScoredResult
	Body        string
	SHA256      string
	StartOffset int
	EndOffset   int
	TotalBytes  int
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
	NextCursor  string
}

func (p expansionPage) Complete() bool { return p.EndOffset == p.TotalBytes }

func NewServer(r *retrieve.Retriever, log *slog.Logger) *Server {
	return NewServerWithFeedback(r, nil, log)
}

func NewServerWithFeedback(r *retrieve.Retriever, rec *feedback.Recorder, log *slog.Logger) *Server {
	return NewServerWithOptions(r, rec, Options{}, log)
}

func NewServerWithOptions(r *retrieve.Retriever, rec *feedback.Recorder, opts Options, log *slog.Logger) *Server {
	return &Server{retriever: r, feedback: rec, options: opts, log: log}
}

func (s *Server) Serve(ctx context.Context) error {
	srv := mcpserver.NewMCPServer("contextmaxxer", "0.1.0")

	tool := mcp.NewTool("find_context",
		mcp.WithDescription(findContextToolDescription),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Either a position or a sentence. A position — path/to/file.go:142, or a whole grep output line pasted verbatim — is looked up, not searched, and returns the symbol enclosing that line with its callers and callees; use it whenever you already have a hit from grep. Otherwise: one sentence naming the mechanism you are looking for, in the vocabulary the code would use. Do NOT paste a whole issue, stack trace or log: measured on 493 tasks, the title alone beat the full report by 17% precision. Do not go to the other extreme either — bare identifiers score worse than the full report, because the match needs a phrase, not a bag of names."),
		),
		mcp.WithString("mode",
			mcp.Description("Output detail level: 'answer' (default - top-5 + graph context + confidence), 'minimal' (just code bodies, ~50% tokens), 'explore' (+ package structure + next steps, for unfamiliar codebases)"),
		),
		mcp.WithNumber("budget_tokens",
			mcp.Description("Max total tokens to return"),
		),
		mcp.WithNumber("max_results",
			mcp.Description("Max number of symbols (default 5; recall saturates early, so raising this mostly adds tokens — rephrase instead if the answer is missing)"),
		),
		mcp.WithNumber("rerank_k",
			mcp.Description("Candidate count to rerank before returning max_results"),
		),
		mcp.WithNumber("seed_k",
			mcp.Description("Seed candidates pulled from vector+FTS before graph expansion (0 = default)"),
		),
		mcp.WithBoolean("skip_intent",
			mcp.Description("Disable the symbolic intent ranker (experiment knob)"),
		),
		mcp.WithBoolean("preserve_full_bodies",
			mcp.Description("Skip query-relevant evidence trimming and return whole indexed excerpts (experiment knob)"),
		),
		mcp.WithNumber("test_floor",
			mcp.Description("Reserve this many top answer slots for non-test files; tests keep their order behind them and fill what is left (experiment knob)"),
		),
		mcp.WithNumber("literal_slots",
			mcp.Description("Hand this many of the last answer slots to files where several of the query's identifiers occur together (experiment knob; needs an index built with --include-tests to have anything new to offer)"),
		),
		mcp.WithBoolean("adaptive_rerank",
			mcp.Description("Skip cross-encoder rerank for confident exact/constructor top matches"),
		),
		mcp.WithString("format",
			mcp.Description("Response encoding: 'md' (default — markdown cards, cheapest to read) or 'json'"),
		),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query, err := req.RequireString("query")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		modeStr := req.GetString("mode", "answer")
		outputMode, err := retrieve.ParseOutputMode(modeStr)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		budgetTokens := req.GetInt("budget_tokens", 0)
		// DECISION(2026-06): default cut from 30. Recall saturates early (R@5≈0.93,
		// R@10≈0.98 on the gen corpus), so 30 was pure token overhead — the agent
		// A/B showed default output dominating context cost. Now releasecfg
		// owns the value (5 after the intent-ranker campaign; see the
		// DECISION there for the measured hit/token tradeoff).
		maxResults := req.GetInt("max_results", releasecfg.MaxResults)
		// Default 15, matching the pool every published gen-eval number was
		// measured with; 0 collapsed the rerank pool to max_results (8), so
		// the cross-encoder could never promote a rank 9-15 seed.
		rerankK := req.GetInt("rerank_k", releasecfg.RerankK)
		// Experiment hook (0 = pipeline default): the seed pool is the hard
		// ceiling on what any ranking can reach, and its size was calibrated on
		// repos ~30x smaller than the largest indexes we now serve.
		seedK := req.GetInt("seed_k", 0)
		// Experiment hook: add this many 1-hop graph neighbours of the top seeds
		// to the candidate pool. Measured motivation is in fusion.go.
		anchorExpand := req.GetInt("anchor_expand", 0)
		literalSlots := req.GetInt("literal_slots", 0)
		testFloor := req.GetInt("test_floor", 0)
		// Experiment hook: the weight between lexical/vector seeds and
		// personalized PageRank. 1.0 is seeds only, which is how much of the
		// answer the graph is responsible for — a question the observational
		// data cannot settle, since graph density tracks language.
		var alpha float64
		alphaSet := false
		if s := req.GetString("alpha", ""); s != "" {
			if v, err := strconv.ParseFloat(s, 64); err == nil && v >= 0 && v <= 1 {
				alpha, alphaSet = v, true
			}
		}
		// Experiment hook: turn the intent ranker off to measure what it
		// actually contributes on repos we did not write.
		skipIntent := req.GetBool("skip_intent", false)
		// Experiment hook: evidence trimming costs 0.8-2.8s on cockroach (33-55%
		// of the query) because it embeds every 10-line window of every result.
		// expand_context now covers the case it was invented for, so its value
		// needs re-measuring rather than assuming.
		preserveFullBodies := req.GetBool("preserve_full_bodies", false)
		adaptiveRerank := req.GetBool("adaptive_rerank", s.options.AdaptiveRerank)
		// Experiment hook (0 = the session-aware default below, -1 = every result
		// keeps its body). How many bodies travel is the single biggest lever on
		// what the answer actually contains: the default sends three, so a
		// twenty-result response is about sixty lines of code.
		fullBodies := req.GetInt("full_body_results", 0)
		if fullBodies == 0 {
			// Session-aware compaction: deep into a
			// session the default tightens to full body for the top result only.
			callN := s.calls.Add(1)
			if callN > sessionCompactAfter {
				fullBodies = 1
			}
		}
		// DECISION(2026-07): markdown is the default encoding — the payload is
		// read by an LLM, and JSON spends ~25% of it on quotes/braces/escapes.
		// Debug sessions default to json (that's where the machine fields are).
		format := req.GetString("format", "")
		if format == "" {
			if os.Getenv("CONTEXTMAXXER_MCP_DEBUG") != "" {
				format = "json"
			} else {
				format = "md"
			}
		}
		requestID := feedback.NewRequestID()

		result, err := s.retriever.Retrieve(ctx, retrieve.Request{
			Query:              query,
			BudgetTokens:       budgetTokens,
			MaxResults:         maxResults,
			RerankK:            rerankK,
			SeedK:              seedK,
			AnchorExpand:       anchorExpand,
			LiteralSlots:       literalSlots,
			TestFloor:          testFloor,
			Alpha:              float32(alpha),
			AlphaSet:           alphaSet,
			SkipIntent:         skipIntent,
			PreserveFullBodies: preserveFullBodies,
			AdaptiveRerank:     adaptiveRerank,
			LazyRerank:         s.options.LazyRerank,
			FullBodyResults:    fullBodies,
			OutputMode:         outputMode,
		})
		if err != nil {
			s.log.Error("retrieve failed", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("retrieve failed: %v", err)), nil
		}
		s.cacheExpansion(requestID, result.ExpansionSymbols)

		type flowRefOut struct {
			Name     string `json:"name"`
			File     string `json:"file"`
			Lines    string `json:"lines"`
			Kind     string `json:"kind"`
			Distance int    `json:"distance"`
			Via      string `json:"via"`
		}
		// DECISION(2026-07): the payload is agent food, not telemetry. A live
		// audit of a 1,287-token answer found ~25-30% spent on things no agent
		// uses: per-symbol related_in_response lists (quadratic duplication of
		// the other results' names), pipeline diagnostics (why/visibility,
		// 8-decimal scores), per-call stats, and a next_steps block that
		// duplicated retrieval_health. Diagnostics stay available behind
		// CONTEXTMAXXER_MCP_DEBUG=1 and always flow into the feedback log.
		type symOut struct {
			File           string         `json:"file"`
			Name           string         `json:"name"`
			Kind           string         `json:"kind"`
			Lines          string         `json:"lines"`
			Score          float32        `json:"score"`
			Relevance      float32        `json:"relevance,omitempty"`
			Confidence     string         `json:"confidence,omitempty"`
			Visibility     string         `json:"visibility,omitempty"`
			Why            string         `json:"why,omitempty"`
			Detail         string         `json:"detail,omitempty"`
			VisibleLines   string         `json:"visible_lines,omitempty"`
			OmittedBefore  int            `json:"omitted_before,omitempty"`
			OmittedAfter   int            `json:"omitted_after,omitempty"`
			Signature      string         `json:"signature,omitempty"`
			Body           string         `json:"body"`
			Callers        []symbolRefOut `json:"callers,omitempty"`
			Callees        []symbolRefOut `json:"callees,omitempty"`
			Tests          []symbolRefOut `json:"tests,omitempty"`
			Siblings       []symbolRefOut `json:"siblings,omitempty"`
			FlowContext    []flowRefOut   `json:"flow_context,omitempty"`
			CompanionFiles []string       `json:"companion_files,omitempty"`
		}
		type retrievalHealthOut struct {
			CandidatesSeen int     `json:"candidates_seen"`
			TopScoreGap    float32 `json:"top_score_gap"`
			TiedCandidates int     `json:"tied_candidates"`
			Confidence     string  `json:"confidence"`
			Suggestion     string  `json:"suggestion,omitempty"`
		}
		type structureOut struct {
			Summary     string              `json:"summary"`
			PackageView map[string][]string `json:"package_view"`
		}
		type nextStepsOut struct {
			IfTopCorrect string   `json:"if_top_correct,omitempty"`
			IfUnsure     string   `json:"if_unsure,omitempty"`
			ToExplore    []string `json:"to_explore,omitempty"`
		}
		type statsOut struct {
			SeedCount     int   `json:"seed_count"`
			GraphNodes    int   `json:"graph_nodes"`
			GraphEdges    int   `json:"graph_edges"`
			PPRIterations int   `json:"ppr_iterations"`
			EmbedMs       int64 `json:"embed_ms"`
			SeedMs        int64 `json:"seed_ms"`
			GraphMs       int64 `json:"graph_ms"`
			PPRMs         int64 `json:"ppr_ms"`
			RerankMs      int64 `json:"rerank_ms"`
			IntentMs      int64 `json:"intent_ms"`
			EscalateMs    int64 `json:"escalate_ms"`
			PackMs        int64 `json:"pack_ms"`
			EvidenceMs    int64 `json:"evidence_ms"`
			GraphCtxMs    int64 `json:"graph_ctx_ms"`
			TotalMs       int64 `json:"total_ms"`
		}
		type output struct {
			RequestID       string              `json:"request_id"`
			Mode            string              `json:"mode"`
			Symbols         []symOut            `json:"symbols"`
			TotalTokens     int                 `json:"total_tokens"`
			Structure       *structureOut       `json:"structure,omitempty"`
			NextSteps       *nextStepsOut       `json:"next_steps,omitempty"`
			RetrievalHealth *retrievalHealthOut `json:"retrieval_health,omitempty"`
			Stats           *statsOut           `json:"stats,omitempty"`
		}
		debugOut := os.Getenv("CONTEXTMAXXER_MCP_DEBUG") != ""

		toFlowRefOut := func(refs []retrieve.FlowRef) []flowRefOut {
			if len(refs) == 0 {
				return nil
			}
			out := make([]flowRefOut, len(refs))
			for i, r := range refs {
				out[i] = flowRefOut{Name: r.QualifiedName, File: r.File, Lines: r.Lines, Kind: r.Kind, Distance: r.Distance, Via: r.Via}
			}
			return out
		}
		syms := make([]symOut, len(result.Symbols))
		candidates := make([]feedback.Candidate, len(result.Symbols))
		for i, sr := range result.Symbols {
			o := symOut{
				File:           sr.File,
				Name:           sr.QualifiedName,
				Kind:           sr.Kind,
				Lines:          fmt.Sprintf("%d-%d", sr.StartLine, sr.EndLine),
				Score:          float32(math.Round(float64(sr.Score)*1000) / 1000),
				Relevance:      float32(math.Round(float64(sr.Relevance)*1000) / 1000),
				Confidence:     sr.Confidence,
				Body:           stripCR(sr.Body),
				CompanionFiles: sr.CompanionFiles,
			}
			// "compact" and "excerpt" tell the agent that Body is incomplete;
			// "full" is the default assumption and not worth the tokens.
			if sr.Detail != "full" {
				o.Detail = sr.Detail
			}
			// Both incomplete forms report what is visible. "compact" used to
			// omit it, which left `lines` — the symbol's full extent — as the
			// only span in the response, and that is not what was sent.
			if sr.Detail == "excerpt" || sr.Detail == "compact" {
				o.VisibleLines = visibleSpanText(sr)
				o.OmittedBefore = sr.BodyStartLine - sr.StartLine
				o.OmittedAfter = sr.EndLine - sr.BodyEndLine
			}
			if debugOut {
				o.Visibility = sr.Visibility
				o.Why = sr.Why
			}
			o.Signature = sr.Signature
			o.Callers = symbolRefOutputs(sr.Callers)
			o.Callees = symbolRefOutputs(sr.Callees)
			o.Tests = symbolRefOutputs(sr.Tests)
			o.Siblings = symbolRefOutputs(sr.Siblings)
			o.CompanionFiles = sr.CompanionFiles
			// DECISION(2026-08): graph refs stay on compact results. The paired
			// Cockroach subagent A/B showed that dropping them after session
			// compaction turned one trace into 97 semantic searches; one-line refs
			// are the navigation payload, while bodies are the expensive tier.
			// REVISIT IF: measured compact-tail refs dominate response tokens.
			if sr.Detail != "compact" {
				// Number the body lines with their real file line numbers so the
				// agent can cite file:line straight from the result instead of
				// re-opening the file to confirm (the confirmation Read was a
				// measured cost in the agent A/B). BodyStartLine accounts for an
				// evidence-span trim that moved the window down. Compact-tier
				// bodies are not real source spans, so they stay unnumbered.
				start := sr.BodyStartLine
				if start == 0 {
					start = sr.StartLine
				}
				o.Body = numberBody(sr.Body, start, sr.BodySegments)
			}
			// flow_context (2-hop call paths) is the fattest part of the payload
			// and remains explore-only even when one-hop refs are retained.
			if outputMode == retrieve.OutputModeExplore {
				o.FlowContext = toFlowRefOut(sr.FlowContext)
			}
			syms[i] = o
			candidates[i] = feedback.Candidate{
				Rank:          i + 1,
				File:          sr.File,
				QualifiedName: sr.QualifiedName,
				Kind:          sr.Kind,
				Score:         sr.Score,
				Why:           sr.Why,
				Features:      sr.Features.AsMap(),
			}
		}

		if err := s.recordRetrieval(ctx, feedback.RetrievalEvent{
			RequestID:   requestID,
			Query:       query,
			Candidates:  candidates,
			TotalTokens: result.TotalTokens,
			Source:      "mcp",
		}); err != nil {
			s.log.Warn("record retrieval feedback failed", "err", err)
		}

		var structOut *structureOut
		if result.Structure != nil {
			structOut = &structureOut{
				Summary:     result.Structure.Summary,
				PackageView: result.Structure.PackageView,
			}
		}
		var nsOut *nextStepsOut
		if debugOut && result.NextSteps != nil {
			nsOut = &nextStepsOut{
				IfTopCorrect: result.NextSteps.IfTopCorrect,
				IfUnsure:     result.NextSteps.IfUnsure,
				ToExplore:    result.NextSteps.ToExplore,
			}
		}
		// retrieval_health earns its tokens only when it has something to say:
		// the low-confidence "rephrase" nudge is the one measured agent-coaching
		// channel (reformulation lifted Hit@1 0.70->0.83).
		var rhOut *retrievalHealthOut
		if rh := result.RetrievalHealth; rh != nil && (debugOut || rh.Confidence == "low") {
			rhOut = &retrievalHealthOut{
				CandidatesSeen: rh.CandidatesSeen,
				TopScoreGap:    rh.TopScoreGap,
				TiedCandidates: rh.TiedCandidates,
				Confidence:     rh.Confidence,
				Suggestion:     rh.Suggestion,
			}
		}

		out := output{
			RequestID:       requestID,
			Mode:            outputMode.String(),
			Symbols:         syms,
			TotalTokens:     result.TotalTokens,
			Structure:       structOut,
			NextSteps:       nsOut,
			RetrievalHealth: rhOut,
		}
		if debugOut {
			st := result.Stats
			out.Stats = &statsOut{
				SeedCount:     st.SeedCount,
				GraphNodes:    st.GraphNodes,
				GraphEdges:    st.GraphEdges,
				PPRIterations: st.PPRIterations,
				EmbedMs:       st.EmbedDuration.Milliseconds(),
				SeedMs:        st.SeedDuration.Milliseconds(),
				GraphMs:       st.GraphDuration.Milliseconds(),
				PPRMs:         st.PPRDuration.Milliseconds(),
				RerankMs:      st.RerankDuration.Milliseconds(),
				IntentMs:      st.IntentDuration.Milliseconds(),
				EscalateMs:    st.EscalateDuration.Milliseconds(),
				PackMs:        st.PackDuration.Milliseconds(),
				EvidenceMs:    st.EvidenceDuration.Milliseconds(),
				GraphCtxMs:    st.GraphCtxDuration.Milliseconds(),
				TotalMs:       st.Total.Milliseconds(),
			}
		}

		if format == "md" {
			return mcp.NewToolResultText(renderMarkdown(requestID, outputMode, result)), nil
		}
		data, err := json.Marshal(out)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("marshal result: %v", err)), nil
		}
		return mcp.NewToolResultText(string(data)), nil
	})

	expandTool := mcp.NewTool("expand_context",
		mcp.WithDescription(`Hydrate one exact indexed symbol body from a prior find_context response. This does not embed, rerank, or repeat semantic search. If the body exceeds one safe response, status:more includes next_cursor; call continue_context until status:complete. A partial page is never a full body and must not be replaced by grep or file reads.`),
		mcp.WithString("request_id",
			mcp.Required(),
			mcp.Description("request_id returned by find_context"),
		),
		mcp.WithNumber("rank",
			mcp.Required(),
			mcp.Description("One-based find_context result rank to hydrate"),
		),
	)
	srv.AddTool(expandTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		requestID, err := req.RequireString("request_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		rank := req.GetInt("rank", 0)
		if rank < 1 {
			return mcp.NewToolResultError("rank must be a positive one-based result rank"), nil
		}
		page, err := s.startExpansion(ctx, requestID, rank)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(renderExpansionPage(page)), nil
	})

	relatedTool := mcp.NewTool("find_related_edits",
		mcp.WithDescription(`Call this AFTER making an edit, before reporting the work done: it reports where else the names you just changed already live, which is how a change that belongs in several places is caught. Paste the diff you produced, or name the identifiers you renamed or introduced. Ranking is by rarity, so a name carried by most of the repository is ignored and a name carried by two symbols decides the answer. This runs no embedding and no semantic search.`),
		mcp.WithString("changed",
			mcp.Required(),
			mcp.Description("The diff you just made, pasted verbatim, or the identifiers the edit touched. From a diff only added and removed lines are read: context lines describe code that stayed the same."),
		),
		mcp.WithString("exclude",
			mcp.Description("Comma-separated paths you have already edited, so they are not offered back to you."),
		),
		mcp.WithNumber("max_results",
			mcp.Description("How many related files to return (default 5)"),
		),
	)
	srv.AddTool(relatedTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		changed, err := req.RequireString("changed")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var exclude []string
		for _, p := range strings.Split(req.GetString("exclude", ""), ",") {
			if p = strings.TrimSpace(p); p != "" {
				exclude = append(exclude, p)
			}
		}
		limit := req.GetInt("max_results", 5)
		related, err := s.retriever.FindRelatedEdits(ctx, []string{changed}, exclude, limit)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(renderRelated(related)), nil
	})

	continueTool := mcp.NewTool("continue_context",
		mcp.WithDescription(`Return the next lossless page from expand_context. Call this whenever an expansion returns status:more, using next_cursor verbatim, until status:complete. It performs no search and skipping it leaves the symbol body incomplete.`),
		mcp.WithString("cursor",
			mcp.Required(),
			mcp.Description("Opaque next_cursor returned by expand_context or continue_context"),
		),
	)
	srv.AddTool(continueTool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cursor, err := req.RequireString("cursor")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		page, err := s.continueExpansion(cursor)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(renderExpansionPage(page)), nil
	})

	feedbackTool := mcp.NewTool("record_feedback",
		mcp.WithDescription(`Record which find_context results were useful, so ranking improves for this project. Pass the find_context request_id, the names you used (selected_symbols), and any irrelevant ones (rejected_symbols). Call once after using a find_context result.`),
		mcp.WithString("request_id",
			mcp.Required(),
			mcp.Description("request_id returned by find_context"),
		),
		mcp.WithArray("selected_symbols",
			mcp.WithStringItems(),
			mcp.Description("Symbols that were useful or used"),
		),
		mcp.WithArray("rejected_symbols",
			mcp.WithStringItems(),
			mcp.Description("Symbols that were irrelevant or misleading"),
		),
		mcp.WithString("query",
			mcp.Description("Original query, if available"),
		),
		mcp.WithString("outcome",
			mcp.Description("Short label such as used, partial, rejected, fixed, failed"),
		),
		mcp.WithString("note",
			mcp.Description("Optional human-readable reason"),
		),
	)
	srv.AddTool(feedbackTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		requestID, err := req.RequireString("request_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if s.feedback == nil {
			return mcp.NewToolResultError("feedback recording is disabled"), nil
		}

		event := feedback.FeedbackEvent{
			RequestID:       requestID,
			Query:           req.GetString("query", ""),
			SelectedSymbols: req.GetStringSlice("selected_symbols", nil),
			RejectedSymbols: req.GetStringSlice("rejected_symbols", nil),
			Outcome:         req.GetString("outcome", ""),
			Note:            req.GetString("note", ""),
			Source:          "mcp",
		}
		if err := s.recordFeedback(ctx, event); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("record feedback: %v", err)), nil
		}
		return mcp.NewToolResultText(`{"ok":true}`), nil
	})

	// DECISION: StdioServer writes JSON-RPC to stdout; all our logs go to stderr
	// via the slog handler initialized in RunMCP. We set an explicit error logger
	// pointing to stderr so mcp-go's internal errors also avoid polluting stdout.
	stdioSrv := mcpserver.NewStdioServer(srv)

	s.log.Info("MCP server started", "transport", "stdio")
	return stdioSrv.Listen(ctx, os.Stdin, os.Stdout)
}

func (s *Server) cacheExpansion(requestID string, symbols []retrieve.ScoredResult) {
	if requestID == "" || len(symbols) == 0 {
		return
	}
	targets := make([]expansionTarget, len(symbols))
	for i, symbol := range symbols {
		targets[i] = expansionTarget{
			SymbolID: symbol.SymbolID, File: symbol.File, QualifiedName: symbol.QualifiedName,
			Kind: symbol.Kind, StartLine: symbol.StartLine, EndLine: symbol.EndLine,
		}
	}

	s.expansionMu.Lock()
	defer s.expansionMu.Unlock()
	if s.expansionCache == nil {
		s.expansionCache = make(map[string][]expansionTarget)
	}
	// DECISION(2026-08): request IDs remain expandable for the lifetime of the MCP
	// process; cache only stable symbol identity/render metadata, never bodies or
	// ranking evidence. ASSUMES: a host session issues far fewer than 100k searches.
	// REVISIT IF: session memory profiles show this metadata map is material.
	s.expansionCache[requestID] = targets
}

func (s *Server) cachedRank(requestID string, rank int) (expansionTarget, error) {
	s.expansionMu.Lock()
	defer s.expansionMu.Unlock()
	cached, ok := s.expansionCache[requestID]
	if !ok {
		return expansionTarget{}, fmt.Errorf("unknown request_id %q; rerun find_context (the MCP server may have restarted)", requestID)
	}
	if rank < 1 || rank > len(cached) {
		return expansionTarget{}, fmt.Errorf("rank %d out of range; request has %d results", rank, len(cached))
	}
	return cached[rank-1], nil
}

func (s *Server) startExpansion(ctx context.Context, requestID string, rank int) (expansionPage, error) {
	target, err := s.cachedRank(requestID, rank)
	if err != nil {
		return expansionPage{}, err
	}
	if s.retriever == nil || target.SymbolID == 0 {
		return expansionPage{}, fmt.Errorf("rank %d cannot be hydrated: missing indexed symbol identity", rank)
	}
	body, err := s.retriever.GetSymbolBody(ctx, target.SymbolID)
	if err != nil {
		return expansionPage{}, fmt.Errorf("hydrate rank %d: %w", rank, err)
	}
	return s.pageFromState(continuationState{
		RequestID: requestID,
		Rank:      rank,
		Symbol: retrieve.ScoredResult{
			SymbolID: target.SymbolID, File: target.File, QualifiedName: target.QualifiedName,
			Kind: target.Kind, StartLine: target.StartLine, EndLine: target.EndLine,
		},
		Body:   body.Body,
		SHA256: body.SHA256,
	}), nil
}

func (s *Server) continueExpansion(cursor string) (expansionPage, error) {
	s.expansionMu.Lock()
	state, ok := s.continuations[cursor]
	s.expansionMu.Unlock()
	if !ok {
		return expansionPage{}, fmt.Errorf("unknown or expired continuation cursor %q; rerun expand_context", cursor)
	}
	return s.pageFromState(state), nil
}

func (s *Server) pageFromState(state continuationState) expansionPage {
	start := state.Offset
	end := expansionPageEnd(state.Body, start)
	startLine, startColumn := sourcePosition(state.Body, start, state.Symbol.StartLine)
	endLine, endColumn := sourcePosition(state.Body, end, state.Symbol.StartLine)
	page := expansionPage{
		RequestID:   state.RequestID,
		Rank:        state.Rank,
		Symbol:      state.Symbol,
		Body:        state.Body[start:end],
		SHA256:      state.SHA256,
		StartOffset: start,
		EndOffset:   end,
		TotalBytes:  len(state.Body),
		StartLine:   startLine,
		StartColumn: startColumn,
		EndLine:     endLine,
		EndColumn:   endColumn,
	}
	if end < len(state.Body) {
		page.NextCursor = s.cacheContinuation(continuationState{
			RequestID: state.RequestID,
			Rank:      state.Rank,
			Symbol:    state.Symbol,
			Body:      state.Body,
			SHA256:    state.SHA256,
			Offset:    end,
		})
	}
	return page
}

func (s *Server) cacheContinuation(state continuationState) string {
	cursor := feedback.NewRequestID()
	s.expansionMu.Lock()
	defer s.expansionMu.Unlock()
	if s.continuations == nil {
		s.continuations = make(map[string]continuationState, continuationCacheLimit)
	}
	s.continuations[cursor] = state
	s.continuationOrder = append(s.continuationOrder, cursor)
	for len(s.continuationOrder) > continuationCacheLimit {
		oldest := s.continuationOrder[0]
		s.continuationOrder = s.continuationOrder[1:]
		delete(s.continuations, oldest)
	}
	return cursor
}

func expansionPageEnd(body string, start int) int {
	if start >= len(body) {
		return len(body)
	}
	end := start + expansionPageBytes
	if end >= len(body) {
		return len(body)
	}
	if newline := strings.LastIndexByte(body[start:end], '\n'); newline >= expansionPageBytes/2 {
		return start + newline + 1
	}
	for end > start && !utf8.ValidString(body[start:end]) {
		end--
	}
	return end
}

func sourcePosition(body string, offset, firstLine int) (line, column int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(body) {
		offset = len(body)
	}
	prefix := body[:offset]
	line = firstLine + strings.Count(prefix, "\n")
	lastNewline := strings.LastIndexByte(prefix, '\n')
	column = utf8.RuneCountInString(prefix[lastNewline+1:]) + 1
	return line, column
}

func (s *Server) recordRetrieval(ctx context.Context, event feedback.RetrievalEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.feedback.RecordRetrieval(event)
}

func (s *Server) recordFeedback(ctx context.Context, event feedback.FeedbackEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.feedback.RecordFeedback(event)
}

// numberLines prefixes each line of body with its real file line number
// (starting at startLine), in the number→content style of file readers, so the
// agent can cite an exact file:line straight from the result without re-opening
// the file. Empty bodies pass through unchanged.
// renderMarkdown encodes the response as agent-facing markdown cards. Same
// information as the JSON encoding at ~25-30% fewer tokens (no quote/brace/
// escape overhead, no per-field names), and models read fenced code more
// reliably than JSON-escaped strings. Diagnostics live in format:"json".
func renderMarkdown(requestID string, mode retrieve.OutputMode, result retrieve.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "req:%s (pass to record_feedback)\n", requestID)
	// The caveat is stated once. Repeated on every graph line it measured at 84
	// of 1028 response tokens on cockroach — a tenth of the payload restating a
	// sentence the reader has already taken in (cmd/rspbreak).
	if anyGraphRefs(result) {
		b.WriteString("callers/callees are static candidates (path_status=static_unverified): verify the branch or dispatch before claiming a runtime path.\n")
	}

	slash := func(p string) string { return strings.ReplaceAll(p, "\\", "/") }
	// The cap is silent otherwise: a symbol with thirty callees showed five and
	// said nothing, so the hop an agent came for could vanish without a trace.
	writeRefs := func(label string, refs []retrieve.SymbolRef, total int) {
		if len(refs) == 0 {
			return
		}
		if total > len(refs) {
			label = fmt.Sprintf("%s (%d of %d)", label, len(refs), total)
		}
		parts := make([]string, len(refs))
		for i, r := range refs {
			s := fmt.Sprintf("%s (%s:%s", r.QualifiedName, slash(r.File), r.Lines)
			if r.CallLine > 0 {
				s += fmt.Sprintf("@%d", r.CallLine)
			}
			parts[i] = s + ")"
			if r.CallSite != "" {
				parts[i] += fmt.Sprintf(" [callsite: %s]", r.CallSite)
			}
		}
		fmt.Fprintf(&b, "%s: %s\n", label, strings.Join(parts, "; "))
	}

	for i, sr := range result.Symbols {
		fmt.Fprintf(&b, "\n%d. **%s** (%s) %s:%d-%d",
			i+1, sr.QualifiedName, sr.Kind, slash(sr.File), sr.StartLine, sr.EndLine)
		if sr.Confidence != "" {
			fmt.Fprintf(&b, " [%s]", sr.Confidence)
		}
		if sr.Detail == "compact" {
			teaser := strings.Join(strings.Fields(stripCR(sr.Body)), " ")
			if len(teaser) > 160 {
				teaser = teaser[:160] + "…"
			}
			fmt.Fprintf(&b, " — %s\n", teaser)
		} else {
			if sr.Detail == "excerpt" {
				// Say how much was dropped, not just what survived. A question
				// that needs every branch of a long function ("what disables
				// this path") is answered wrongly from a window, and the agent
				// cannot know to expand unless the loss is on the page: the
				// multi-hop A/B lost exactly one answer this way, naming two of
				// seven conditions from a 110-line planner function.
				fmt.Fprintf(&b, " [excerpt %s — %d of %d lines; expand rank %d for the rest]",
					visibleSpanText(sr), visibleLineCount(sr), sr.EndLine-sr.StartLine+1, i+1)
			}
			b.WriteByte('\n')
			start := sr.BodyStartLine
			if start == 0 {
				start = sr.StartLine
			}
			fmt.Fprintf(&b, "```\n%s\n```\n", numberLines(sr.Body, start))
		}
		writeRefs("callers", sr.Callers, sr.CallersTotal)
		writeRefs("callees", sr.Callees, sr.CalleesTotal)
		writeRefs("tests", sr.Tests, len(sr.Tests))
		writeRefs("siblings", sr.Siblings, len(sr.Siblings))
		if len(sr.CompanionFiles) > 0 {
			fmt.Fprintf(&b, "companions: %s\n", strings.Join(sr.CompanionFiles, "; "))
		}
		if mode == retrieve.OutputModeExplore && len(sr.FlowContext) > 0 {
			parts := make([]string, len(sr.FlowContext))
			for j, fr := range sr.FlowContext {
				parts[j] = fmt.Sprintf("%s (%s:%s, %s)", fr.QualifiedName, slash(fr.File), fr.Lines, fr.Via)
			}
			fmt.Fprintf(&b, "flow: %s\n", strings.Join(parts, "; "))
		}
	}

	if rh := result.RetrievalHealth; rh != nil && rh.Confidence == "low" && rh.Suggestion != "" {
		fmt.Fprintf(&b, "\nnote: low retrieval confidence — %s\n", rh.Suggestion)
	}
	if mode == retrieve.OutputModeExplore && result.Structure != nil {
		fmt.Fprintf(&b, "\n## structure\n%s\n", result.Structure.Summary)
		for pkg, syms := range result.Structure.PackageView {
			fmt.Fprintf(&b, "- %s: %s\n", pkg, strings.Join(syms, ", "))
		}
	}
	return b.String()
}

func renderExpansionPage(page expansionPage) string {
	var b strings.Builder
	status := "more"
	if page.Complete() {
		status = "complete"
	}
	fmt.Fprintf(&b, "status:%s\n", status)
	fmt.Fprintf(&b, "expanded req:%s rank:%d\n", page.RequestID, page.Rank)
	fmt.Fprintf(&b, "symbol: **%s** (%s) %s:%d-%d\n",
		page.Symbol.QualifiedName, page.Symbol.Kind, strings.ReplaceAll(page.Symbol.File, "\\", "/"), page.Symbol.StartLine, page.Symbol.EndLine)
	fmt.Fprintf(&b, "coverage_bytes:%d-%d/%d source:%d:%d-%d:%d sha256:%s\n",
		page.StartOffset, page.EndOffset, page.TotalBytes,
		page.StartLine, page.StartColumn, page.EndLine, page.EndColumn, page.SHA256)
	fmt.Fprintf(&b, "```\n%s\n```\n", numberExpansionLines(page.Body, page.StartLine, page.StartColumn))
	if page.NextCursor != "" {
		fmt.Fprintf(&b, "NEXT ACTION REQUIRED: call continue_context with cursor=%s; this body is not complete.\n", page.NextCursor)
		fmt.Fprintf(&b, "status:more next_cursor:%s\n", page.NextCursor)
	} else {
		b.WriteString("status:complete; the exact indexed body has been delivered.\n")
	}
	return b.String()
}

func numberExpansionLines(body string, startLine, startColumn int) string {
	if body == "" {
		return body
	}
	lines := strings.Split(body, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if i == 0 && startColumn > 1 {
			fmt.Fprintf(&b, "%d:%d\t%s", startLine, startColumn, line)
		} else {
			fmt.Fprintf(&b, "%d\t%s", startLine+i, line)
		}
	}
	return b.String()
}

func numberLines(body string, startLine int) string {
	return numberBody(body, startLine, nil)
}

// numberBody numbers a body with its real file lines. With segments the body
// carries several windows joined by the gap marker, and the counter has to jump
// at each marker: numbering straight through would put a confident, wrong line
// number on every line after the first gap, which is worse than not numbering
// at all — the agent cites those numbers.
func numberBody(body string, startLine int, segments []retrieve.BodySegment) string {
	if body == "" {
		return body
	}
	lines := strings.Split(stripCR(body), "\n")
	var b strings.Builder
	lineNo := startLine
	seg := 0
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if ln == retrieve.EvidenceGapMarker {
			b.WriteString(ln)
			seg++
			if seg < len(segments) {
				lineNo = segments[seg].StartLine
			}
			continue
		}
		fmt.Fprintf(&b, "%d\t%s", lineNo, ln)
		lineNo++
	}
	return b.String()
}

// stripCR removes carriage returns from indexed bodies. Windows checkouts
// carry CRLF endings, and each \r costs two characters of JSON escaping in
// every body line of every response.
func stripCR(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}

// visibleSpanText names the lines the response actually shows: one range, or
// one per window when the trim kept several. Reporting only first-to-last would
// claim the gaps are visible.
func visibleSpanText(sr retrieve.ScoredResult) string {
	if len(sr.BodySegments) < 2 {
		return fmt.Sprintf("%d-%d", sr.BodyStartLine, sr.BodyEndLine)
	}
	parts := make([]string, len(sr.BodySegments))
	for i, seg := range sr.BodySegments {
		parts[i] = fmt.Sprintf("%d-%d", seg.StartLine, seg.StartLine+seg.Lines-1)
	}
	return strings.Join(parts, ",")
}

// anyGraphRefs reports whether the response carries call-graph references, so
// the caveat about them is emitted only when there is something to caveat.
func anyGraphRefs(result retrieve.Result) bool {
	for _, sr := range result.Symbols {
		if len(sr.Callers) > 0 || len(sr.Callees) > 0 {
			return true
		}
	}
	return false
}

// visibleLineCount is how many lines of the symbol the response actually
// carries: the sum of the windows when the trim kept several, otherwise the
// single span.
func visibleLineCount(sr retrieve.ScoredResult) int {
	if len(sr.BodySegments) > 0 {
		n := 0
		for _, seg := range sr.BodySegments {
			n += seg.Lines
		}
		return n
	}
	if sr.BodyEndLine >= sr.BodyStartLine {
		return sr.BodyEndLine - sr.BodyStartLine + 1
	}
	return 0
}

// renderRelated writes the related-edit answer. It names the evidence — which
// rare identifier put each file on the list — because the caller has to judge
// whether the relationship is real, and a bare list of paths gives it nothing
// to judge with. An empty result says so plainly rather than returning nothing:
// silence reads as "the tool failed", while "no other place carries these
// names" is a finding the agent can act on.
func renderRelated(related []retrieve.RelatedFile) string {
	if len(related) == 0 {
		return "no other indexed file carries the names you changed: this edit looks self-contained.\n"
	}
	var b strings.Builder
	b.WriteString("other places carrying the names you changed, rarest name first:\n")
	for i, r := range related {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, strings.ReplaceAll(r.File, "\\", "/"))
		if len(r.Shared) > 0 {
			fmt.Fprintf(&b, "   shares: %s\n", strings.Join(r.Shared, ", "))
		}
		if len(r.Symbols) > 0 {
			fmt.Fprintf(&b, "   in: %s\n", strings.Join(r.Symbols, ", "))
		}
	}
	b.WriteString("\nthese are candidates, not instructions: open one before changing it.\n")
	return b.String()
}
