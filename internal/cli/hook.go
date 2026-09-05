package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
		// ToolResponse is whatever the gated tool returned. Read as raw JSON
		// because Grep's shape depends on its output_mode, and post-search only
		// needs a path and a line out of it.
		ToolResponse json.RawMessage `json:"tool_response"`
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
		// The gate blocks until find_context has answered once. It must block
		// AT MOST ONCE, because the marker is written by a PostToolUse hook and
		// that hook does not run when the tool call FAILS — verified live: a
		// find_context against a corrupt index left no marker. Blocking on every
		// search would then lock the agent out of both tools for the rest of the
		// session, which is the one failure this gate must never cause.
		blocked := marker + ".blocked"
		if _, err := os.Stat(blocked); err == nil {
			recordSearchAfterContext()
			return 0
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
		_ = os.WriteFile(blocked, []byte("1"), 0o644)
		fmt.Fprintln(os.Stderr, hookBlockMessage)
		return 2
	case "post-search":
		// The agent has just run grep and is holding a position. That is the one
		// moment this tool answers without embedding, reranking or a phrase to
		// invent — and until the locator branch existed there was no way to ask
		// it about a position at all.
		//
		// Fires once per session: the nudge is worth making when the agent first
		// has a hit to hand over, and worth nothing repeated after every grep.
		if marker == "" {
			return 0
		}
		nudged := marker + ".nudged"
		if _, err := os.Stat(nudged); err == nil {
			return 0
		}
		loc := firstLocator(in.ToolResponse)
		if loc == "" {
			return 0 // no path in the output: nothing concrete to suggest
		}
		_ = os.WriteFile(nudged, []byte("1"), 0o644)
		emitAdditionalContext(fmt.Sprintf(
			"That grep hit can be handed straight to find_context: query %q. "+
				"A position is looked up, not searched — it returns the symbol "+
				"enclosing that line with its callers and callees, which grep "+
				"cannot give you, and costs no embedding or rerank.", loc))
		return 0
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
//
// The "one sentence, not a pasted report" clause is measured too, on 493
// SWE-Explore instances: searching an issue's TITLE instead of the whole report
// gained +0.058 precision (+17%) and +36% line recall. Deleting text beat every
// component of the pipeline — the graph is worth +0.020, the cross-encoder
// +0.036. The opposite extreme is worse than either: stripping the query down
// to bare identifiers dropped file reach from 0.515 to 0.434, because the
// embedder needs a phrase, not a bag of names.
const hookBlockMessage = "Use find_context first. This repository has a semantic code-search tool " +
	"(the find_context MCP tool) that locates code in one call with caller/callee graph " +
	"context — call it before grep/glob when you need to find code, understand how something " +
	"works, or trace a call path. Phrase the query in the vocabulary the code would use " +
	"(mechanism nouns/verbs, likely identifier words), one mechanism per query. " +
	"Keep it to ONE SENTENCE: do not paste a whole issue, stack trace or log — " +
	"that measurably costs 17% precision — and do not reduce it to bare identifiers either, " +
	"which is worse still. Name the mechanism in a phrase. " +
	"grep/glob become available after find_context for literal-string or filename " +
	"searches — and when a grep hit matters, paste it straight back as the query " +
	"(\"path/to/file.go:142\", or the whole grep line): that form is an index lookup, " +
	"not a search, and returns the enclosing symbol with its callers and callees, " +
	"which grep cannot give you. [Contextmaxxer discovery gate]"

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

// firstLocator pulls a concrete "path:line" out of a tool response.
//
// Grep's response shape depends on its output_mode (matching lines, file names,
// or counts), and no schema is depended on here: the raw JSON is searched for
// the shapes a text search emits, preferring one that carries a line number
// because that is what makes the suggestion a position rather than a file.
func firstLocator(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	// A Windows path arrives inside JSON as escaped backslashes ("a\\b\\c.go").
	// Matching on the raw bytes would stop at the first one, so separators are
	// normalized to "/" before matching — which is also the form the locator
	// branch normalizes to.
	text := strings.ReplaceAll(string(raw), `\\`, "/")
	if m := rePathLine.FindStringSubmatch(text); m != nil {
		return m[1] + ":" + m[2]
	}
	if m := rePathOnly.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

var (
	// A path with a line number, as `rg -n` prints it. The path must contain a
	// separator: a bare "config.py:3" out of prose would be a guess, and the
	// locator branch refuses anything the index cannot resolve anyway.
	rePathLine = regexp.MustCompile(`([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+\.[A-Za-z0-9]{1,5}):(\d+)`)
	rePathOnly = regexp.MustCompile(`([A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)+\.[A-Za-z0-9]{1,5})`)
)

// emitAdditionalContext writes the PostToolUse JSON that adds a line to the
// agent's context.
//
// VERIFIED(2026-09-04) against a live Claude Code session: the text arrives in
// the model's context immediately after the tool result, on its own line
// prefixed "PostToolUse:Grep hook additional context: ...". Exit 0 with JSON on
// stdout is therefore enough, and the louder alternative — exit 2, whose stderr
// is fed back to the model — stays unused, because it renders a suggestion as a
// tool error.
//
// Getting that verification took four session restarts and turned up the reason
// no hook this project ever installed had run on Windows: see shellCommandPath
// in hostsetup.go. Until that was fixed the hook died at "command not found"
// with a clean exit code, so nothing about this field could be observed at all.
func emitAdditionalContext(msg string) {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "PostToolUse",
			"additionalContext": msg,
		},
	}
	if b, err := json.Marshal(out); err == nil {
		fmt.Println(string(b))
	}
}
