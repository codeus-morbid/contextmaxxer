package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RunHook implements the "discovery gate" used as a Claude Code hook. It returns
// the process exit code directly: 2 blocks a tool call (PreToolUse semantics),
// 0 allows it. It must stay fast and dependency-free — it runs on every gated
// tool call (no DB, no model, no app init).
//
//	contextmaxxer hook pre-search   # PreToolUse on Grep|Glob
//	contextmaxxer hook post-find    # PostToolUse on find_context
//
// The gate forces an agent to lead with find_context: the first Grep/Glob in a
// session is blocked until find_context has been called once, after which
// grep/glob are allowed (for literal-string / filename / refinement searches).
// It fails open — any misconfiguration or missing session id allows the call.
func RunHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: contextmaxxer hook <pre-search|post-find>")
		return 0
	}

	var in struct {
		SessionID string `json:"session_id"`
	}
	if data, err := io.ReadAll(os.Stdin); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &in)
	}
	marker := hookMarkerPath(in.SessionID)

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
			return 0 // gate already opened by a find_context call
		}
		fmt.Fprintln(os.Stderr, hookBlockMessage)
		return 2
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
