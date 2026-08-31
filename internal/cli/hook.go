package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
)

// RunHook implements the "discovery gate" used as a Claude Code hook. It returns
// the process exit code directly: 2 blocks a tool call (PreToolUse semantics),
// 0 allows it. It must stay fast and dependency-free — it runs on every gated
// tool call (no DB, no model, no app init).
//
//	contextmaxxer hook pre-search   # PreToolUse on Grep|Glob
//	contextmaxxer hook post-find    # PostToolUse on find_context (Claude Code)
//	contextmaxxer hook search-signal # a search on Cursor/Codex: record, never block
//
// The gate forces an agent to lead with find_context: the first Grep/Glob in a
// session is blocked until find_context has been called once, after which
// grep/glob are allowed (for literal-string / filename / refinement searches).
// It fails open — any misconfiguration or missing session id allows the call.
func RunHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: contextmaxxer hook <pre-search|post-find|search-signal>")
		return 0
	}

	var in struct {
		// Claude Code and Codex both send session_id; Cursor calls the same
		// thing conversation_id. Reading both is the whole of what it takes to
		// run on all three.
		SessionID      string `json:"session_id"`
		ConversationID string `json:"conversation_id"`
	}
	if data, err := io.ReadAll(os.Stdin); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &in)
	}
	sessionID := in.SessionID
	if sessionID == "" {
		sessionID = in.ConversationID
	}
	marker := hookMarkerPath(sessionID)

	switch args[0] {
	case "post-find":
		if marker != "" {
			_ = os.WriteFile(marker, []byte("1"), 0o644)
		}
		return 0
	case "pre-search":
		if marker == "" {
			return 0 // can't track the session — never block
		}
		if _, err := os.Stat(marker); err == nil {
			// The gate is open, so find_context has already answered in this
			// session and the agent is searching anyway. That is the one signal
			// nobody has to volunteer: record_feedback asks the agent to grade
			// its own result after it already has what it wanted, and across
			// 95958 served responses it was called 57 times. Reaching for grep
			// is not a verdict — it also covers verifying an answer and hunting
			// a literal string — but it is an observation, and observations are
			// what the log was missing.
			recordSearchAfterContext()
			return 0
		}
		fmt.Fprintln(os.Stderr, hookBlockMessage)
		return 2
	case "search-signal":
		// DECISION(2026-08): records the observation and NEVER blocks. The gate
		// needs to know find_context has answered, which on Claude Code comes
		// from a PostToolUse matcher on the MCP tool name. Cursor and Codex can
		// both intercept MCP calls, but their tool-name spelling is not
		// something this was verified against — and a gate that cannot detect
		// find_context never opens, so it would block grep permanently. Signal
		// is safe to ship on an unverified host; blocking is not.
		// REVISIT IF: the MCP matcher is confirmed on a real Cursor/Codex
		// install — then these hosts can use pre-search like Claude Code.
		recordSearchAfterContext()
		return 0
	default:
		return 0
	}
}

// The "rephrase into code vocabulary" line is measured, not folklore: on the
// 30-case gen corpus, agent-reformulated queries lifted Hit@1 0.70->0.83 and
// Hit@3 0.90->1.00 over raw task phrasing; a longer 5-rule style guide added
// nothing on top for a capable model (see memory/query-guide experiment).
const hookBlockMessage = "Use find_context first. This repository has a semantic code-search tool " +
	"(the find_context MCP tool) that locates code in one call with caller/callee graph " +
	"context — call it before grep/glob when you need to find code, understand how something " +
	"works, or trace a call path. Phrase the query in the vocabulary the code would use " +
	"(mechanism nouns/verbs, likely identifier words), one mechanism per query. " +
	"grep/glob become available after find_context for literal-string or filename " +
	"searches. [Contextmaxxer discovery gate]"

// hookMarkerPath returns a per-session marker file path, or "" if there is no
// session id (in which case the gate fails open).
func hookMarkerPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(os.TempDir(), "contextmaxxer-discovery-"+hex.EncodeToString(sum[:8]))
}

// searchAfterContextWindow bounds how stale a retrieval may be and still be
// blamed for a search. A session can sit idle for hours between the answer and
// the next command; attributing a search to a retrieval from before lunch would
// manufacture signal rather than record it.
const searchAfterContextWindow = 5 * time.Minute

// recordSearchAfterContext is best-effort by construction. The hook runs on
// every gated tool call and must stay fast and fail open — a logging problem
// must never delay or block the agent's search, so every error here is dropped
// on purpose.
func recordSearchAfterContext() {
	path := os.Getenv("CONTEXTMAXXER_FEEDBACK_LOG")
	if path == "" {
		path = filepath.Join(".contextmaxxer", "feedback.jsonl")
	}
	if path == "none" {
		return
	}
	_, _ = feedback.RecordObservedOutcome(path, feedback.OutcomeSearchedAfterContext, searchAfterContextWindow)
}
