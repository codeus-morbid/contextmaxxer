package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync/atomic"

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
}

// sessionCompactAfter is the find_context call count past which the default
// full-body count drops to 1. DECISION(2026-07): a 70-stage audit A/B showed
// the default payload (~1-1.5K tokens/call) COMPOUNDING over 68 sequential
// calls into 3x the grep arm's total tokens — the rich default is right for
// the first few calls and wrong for marathon sessions. Explicit full_bodies
// from the caller always wins. REVISIT IF: long-session A/B shows reads
// creeping back up (the graph context, which prevents them, stays intact).
const sessionCompactAfter = 4

type Options struct {
	AdaptiveRerank bool
	// LazyRerank runs the cross-encoder only when the fused ranking is
	// ambiguous, keeping confident queries on the fast (~100ms) path.
	LazyRerank bool
}

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
		mcp.WithDescription(`Semantic code search for this repository. Returns ranked symbols with line-numbered bodies, signature, and caller/callee graph context — each caller/callee carries a call_line (the exact call-site line) so you can follow a call chain without opening files. Cite file:line directly from results.
Modes: 'answer' (default — top matches + graph + confidence), 'minimal' (bodies only, ~50% tokens), 'explore' (+ package overview, for an unfamiliar codebase).
In long multi-call sessions the server auto-compacts output after the first few calls (full body only for the top result; graph context stays); pass full_bodies explicitly if you need more.
After acting on results, call record_feedback once with this call's request_id and the names you used.`),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Natural-language description of what you're looking for"),
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
		mcp.WithBoolean("adaptive_rerank",
			mcp.Description("Skip cross-encoder rerank for confident exact/constructor top matches"),
		),
		mcp.WithNumber("full_bodies",
			mcp.Description("How many top results include the full code body (default 3); the rest carry signature + doc line marked detail:'compact'. Use -1 to get full bodies for everything."),
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
		// Experiment hook: turn the intent ranker off to measure what it
		// actually contributes on repos we did not write.
		skipIntent := req.GetBool("skip_intent", false)
		adaptiveRerank := req.GetBool("adaptive_rerank", s.options.AdaptiveRerank)
		fullBodies := req.GetInt("full_bodies", 0)
		// Session-aware compaction: 0 means "caller didn't ask"; deep into a
		// session the default tightens to full body for the top result only.
		callN := s.calls.Add(1)
		if fullBodies == 0 && callN > sessionCompactAfter {
			fullBodies = 1
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
			Query:           query,
			BudgetTokens:    budgetTokens,
			MaxResults:      maxResults,
			RerankK:         rerankK,
			SeedK:           seedK,
			SkipIntent:      skipIntent,
			AdaptiveRerank:  adaptiveRerank,
			LazyRerank:      s.options.LazyRerank,
			FullBodyResults: fullBodies,
			OutputMode:      outputMode,
		})
		if err != nil {
			s.log.Error("retrieve failed", "err", err)
			return mcp.NewToolResultError(fmt.Sprintf("retrieve failed: %v", err)), nil
		}

		type symRefOut struct {
			Name     string `json:"name"`
			File     string `json:"file"`
			Lines    string `json:"lines"`
			Kind     string `json:"kind"`
			CallLine int    `json:"call_line,omitempty"`
		}
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
			File           string       `json:"file"`
			Name           string       `json:"name"`
			Kind           string       `json:"kind"`
			Lines          string       `json:"lines"`
			Score          float32      `json:"score"`
			Relevance      float32      `json:"relevance,omitempty"`
			Confidence     string       `json:"confidence,omitempty"`
			Visibility     string       `json:"visibility,omitempty"`
			Why            string       `json:"why,omitempty"`
			Detail         string       `json:"detail,omitempty"`
			Signature      string       `json:"signature,omitempty"`
			Body           string       `json:"body"`
			Callers        []symRefOut  `json:"callers,omitempty"`
			Callees        []symRefOut  `json:"callees,omitempty"`
			Tests          []symRefOut  `json:"tests,omitempty"`
			Siblings       []symRefOut  `json:"siblings,omitempty"`
			FlowContext    []flowRefOut `json:"flow_context,omitempty"`
			CompanionFiles []string     `json:"companion_files,omitempty"`
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

		toSymRefOut := func(refs []retrieve.SymbolRef) []symRefOut {
			if len(refs) == 0 {
				return nil
			}
			out := make([]symRefOut, len(refs))
			for i, r := range refs {
				out[i] = symRefOut{Name: r.QualifiedName, File: r.File, Lines: r.Lines, Kind: r.Kind, CallLine: r.CallLine}
			}
			return out
		}

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
			// "compact" tells the agent the body is a teaser, not the source;
			// "full" is the default assumption and not worth the tokens.
			if sr.Detail == "compact" {
				o.Detail = sr.Detail
			}
			if debugOut {
				o.Visibility = sr.Visibility
				o.Why = sr.Why
			}
			// Graph context (callers/callees/tests/siblings/flow) is the bulk of
			// the payload. Attach it only to the full-body tier — that's where the
			// agent actually decides. Compact tail results carry just signature +
			// doc line, keeping responses lean (see agent A/B: default output was
			// the token-cost culprit).
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
				o.Signature = sr.Signature
				o.Body = numberLines(sr.Body, start)
				o.Callers = toSymRefOut(sr.Callers)
				o.Callees = toSymRefOut(sr.Callees)
				o.Tests = toSymRefOut(sr.Tests)
				o.Siblings = toSymRefOut(sr.Siblings)
				// flow_context (2-hop call paths) is the fattest part of the
				// payload and rarely needed to answer — only emit it in explore
				// mode. callers/callees (1-hop, one-line refs) stay in the default.
				if outputMode == retrieve.OutputModeExplore {
					o.FlowContext = toFlowRefOut(sr.FlowContext)
				}
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

	slash := func(p string) string { return strings.ReplaceAll(p, "\\", "/") }
	writeRefs := func(label string, refs []retrieve.SymbolRef) {
		if len(refs) == 0 {
			return
		}
		parts := make([]string, len(refs))
		for i, r := range refs {
			s := fmt.Sprintf("%s (%s:%s", r.QualifiedName, slash(r.File), r.Lines)
			if r.CallLine > 0 {
				s += fmt.Sprintf("@%d", r.CallLine)
			}
			parts[i] = s + ")"
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
			continue
		}
		b.WriteByte('\n')
		start := sr.BodyStartLine
		if start == 0 {
			start = sr.StartLine
		}
		fmt.Fprintf(&b, "```\n%s\n```\n", numberLines(sr.Body, start))
		writeRefs("callers", sr.Callers)
		writeRefs("callees", sr.Callees)
		writeRefs("tests", sr.Tests)
		writeRefs("siblings", sr.Siblings)
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

func numberLines(body string, startLine int) string {
	if body == "" {
		return body
	}
	lines := strings.Split(stripCR(body), "\n")
	var b strings.Builder
	for i, ln := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", startLine+i, ln)
	}
	return b.String()
}

// stripCR removes carriage returns from indexed bodies. Windows checkouts
// carry CRLF endings, and each \r costs two characters of JSON escaping in
// every body line of every response.
func stripCR(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}
