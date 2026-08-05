package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Host config writers: `init` materializes MCP registration and adoption
// rules for each supported agent host instead of printing blocks for a human
// to paste. Design rule: every writer MERGES (never clobbers unrelated keys)
// and is IDEMPOTENT (running init twice changes nothing), so an agent can run
// it blindly. Writers return a human-readable list of files they touched.

const (
	HostClaudeCode = "claude-code"
	HostCursor     = "cursor"
	HostCodex      = "codex"
	HostAuto       = "auto"
)

// adoptionRule is the standing instruction that makes agents lead with
// find_context; kept in sync with BETA.md step 4.
const adoptionRule = `When you need to find code, understand how something works, trace a call path, or
find the right file to edit in this repository, call the find_context MCP tool
FIRST — before grep, glob, or opening files. It returns ranked symbols with
line-numbered bodies and caller/callee context, usually in one call. Phrase the
query in the vocabulary the code would use (mechanism nouns/verbs, likely
identifier words), one mechanism per query. Cite file:line from its output; only
open a file if find_context lacks the detail. Use grep only for literal-string
or filename searches, or when find_context returns nothing.`

func mcpArgs(dbPath string) []string {
	return []string{"mcp", "--index", dbPath, "--reranker", "jina-reranker-v1-tiny-en", "--adaptive-rerank", "--watch"}
}

// DetectHosts reports which agent hosts look present for this repo/user.
func DetectHosts(absRoot string) []string {
	var hosts []string
	if pathExists(filepath.Join(absRoot, ".claude")) || pathExists(filepath.Join(absRoot, ".mcp.json")) {
		hosts = append(hosts, HostClaudeCode)
	}
	if pathExists(filepath.Join(absRoot, ".cursor")) {
		hosts = append(hosts, HostCursor)
	}
	home, err := os.UserHomeDir()
	if (err == nil && pathExists(filepath.Join(home, ".codex"))) || pathExists(filepath.Join(absRoot, "AGENTS.md")) {
		hosts = append(hosts, HostCodex)
	}
	return hosts
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// EnsureGitignore appends ".contextmaxxer/" to the repo's .gitignore when the
// repo is git-tracked and the entry is missing — the index db and veccache are
// multi-MB binary artifacts that `git add .` would otherwise commit.
func EnsureGitignore(absRoot string) (bool, error) {
	if !pathExists(filepath.Join(absRoot, ".git")) {
		return false, nil
	}
	giPath := filepath.Join(absRoot, ".gitignore")
	existing, err := os.ReadFile(giPath)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == ".contextmaxxer/" || trimmed == ".contextmaxxer" || trimmed == "/.contextmaxxer/" || trimmed == "/.contextmaxxer" {
			return false, nil
		}
	}
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return true, os.WriteFile(giPath, []byte(out+".contextmaxxer/\n"), 0644)
}

// WriteHostConfig applies the writer for one host. Returns touched files.
func WriteHostConfig(host, absRoot, cmdPath, dbPath string) ([]string, error) {
	switch host {
	case HostClaudeCode:
		return writeClaudeCode(absRoot, cmdPath, dbPath)
	case HostCursor:
		return writeCursor(absRoot, cmdPath, dbPath)
	case HostCodex:
		return writeCodex(absRoot, cmdPath, dbPath)
	default:
		return nil, fmt.Errorf("unknown host %q (want %s|%s|%s|%s)", host, HostClaudeCode, HostCursor, HostCodex, HostAuto)
	}
}

// --- Claude Code: .mcp.json (repo root) + .claude/settings.json hooks ---

func writeClaudeCode(absRoot, cmdPath, dbPath string) ([]string, error) {
	var touched []string

	mcpPath := filepath.Join(absRoot, ".mcp.json")
	changed, err := mergeMCPServers(mcpPath, cmdPath, dbPath)
	if err != nil {
		return nil, fmt.Errorf("claude-code %s: %w", mcpPath, err)
	}
	if changed {
		touched = append(touched, mcpPath)
	}

	settingsPath := filepath.Join(absRoot, ".claude", "settings.json")
	changed, err = mergeClaudeHooks(settingsPath, cmdPath)
	if err != nil {
		return nil, fmt.Errorf("claude-code %s: %w", settingsPath, err)
	}
	if changed {
		touched = append(touched, settingsPath)
	}
	return touched, nil
}

// mergeMCPServers upserts mcpServers.contextmaxxer, preserving everything else.
func mergeMCPServers(path, cmdPath, dbPath string) (bool, error) {
	doc, err := loadJSONObject(path)
	if err != nil {
		return false, err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	entry := map[string]any{"command": cmdPath, "args": toAnySlice(mcpArgs(dbPath))}
	if existing, ok := servers["contextmaxxer"]; ok && jsonEqual(existing, entry) {
		return false, nil
	}
	servers["contextmaxxer"] = entry
	doc["mcpServers"] = servers
	return true, saveJSONObject(path, doc)
}

// mergeClaudeHooks appends the grep-gate hook pair unless already present
// (matched by the distinctive command substring, so user edits survive).
func mergeClaudeHooks(path, cmdPath string) (bool, error) {
	doc, err := loadJSONObject(path)
	if err != nil {
		return false, err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	add := func(key, matcher, command string) {
		// Presence is detected by the distinctive "hook pre-search"/"hook
		// post-find" tail, not the full command, so a user-edited binary path
		// still counts as wired.
		marker := command[strings.Index(command, " hook ")+1:]
		arr, _ := hooks[key].([]any)
		for _, e := range arr {
			if b, err := json.Marshal(e); err == nil && strings.Contains(string(b), marker) {
				return // already wired; respect whatever form it has
			}
		}
		arr = append(arr, map[string]any{
			"matcher": matcher,
			"hooks":   []any{map[string]any{"type": "command", "command": command}},
		})
		hooks[key] = arr
		changed = true
	}
	// The hook command runs through a shell: a binary path with spaces
	// (e.g. Program Files) must be quoted.
	quoted := cmdPath
	if strings.Contains(quoted, " ") {
		quoted = `"` + quoted + `"`
	}
	add("PreToolUse", "Grep|Glob", quoted+" hook pre-search")
	add("PostToolUse", "mcp__.*__find_context", quoted+" hook post-find")
	if !changed {
		return false, nil
	}
	doc["hooks"] = hooks
	return true, saveJSONObject(path, doc)
}

// --- Cursor: .cursor/mcp.json + .cursor/rules/contextmaxxer.mdc ---

func writeCursor(absRoot, cmdPath, dbPath string) ([]string, error) {
	var touched []string

	mcpPath := filepath.Join(absRoot, ".cursor", "mcp.json")
	changed, err := mergeMCPServers(mcpPath, cmdPath, dbPath)
	if err != nil {
		return nil, fmt.Errorf("cursor %s: %w", mcpPath, err)
	}
	if changed {
		touched = append(touched, mcpPath)
	}

	rulePath := filepath.Join(absRoot, ".cursor", "rules", "contextmaxxer.mdc")
	rule := "---\ndescription: Prefer find_context for code navigation\nalwaysApply: true\n---\n" + adoptionRule + "\n"
	changed, err = writeFileIfDifferent(rulePath, rule)
	if err != nil {
		return nil, fmt.Errorf("cursor %s: %w", rulePath, err)
	}
	if changed {
		touched = append(touched, rulePath)
	}
	return touched, nil
}

// --- Codex: ~/.codex/config.toml + repo AGENTS.md ---

func writeCodex(absRoot, cmdPath, dbPath string) ([]string, error) {
	var touched []string

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("codex: resolve home: %w", err)
	}
	tomlPath := filepath.Join(home, ".codex", "config.toml")
	changed, err := appendCodexServer(tomlPath, cmdPath, dbPath)
	if err != nil {
		return nil, fmt.Errorf("codex %s: %w", tomlPath, err)
	}
	if changed {
		touched = append(touched, tomlPath)
	}

	agentsPath := filepath.Join(absRoot, "AGENTS.md")
	changed, err = appendMarkedBlock(agentsPath,
		"<!-- contextmaxxer:start -->",
		"<!-- contextmaxxer:end -->",
		"## Code search\n\n"+adoptionRule+"\n")
	if err != nil {
		return nil, fmt.Errorf("codex %s: %w", agentsPath, err)
	}
	if changed {
		touched = append(touched, agentsPath)
	}
	return touched, nil
}

// appendCodexServer adds the [mcp_servers.contextmaxxer] TOML table if absent.
// Text-level append on purpose: we never rewrite the user's existing config,
// so a TOML parser dependency is not worth the risk surface. If the section
// exists (whatever its content), we leave it alone.
func appendCodexServer(path, cmdPath, dbPath string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(existing), "[mcp_servers.contextmaxxer]") {
		return false, nil
	}
	var args []string
	for _, a := range mcpArgs(dbPath) {
		args = append(args, fmt.Sprintf("%q", a))
	}
	block := fmt.Sprintf("\n[mcp_servers.contextmaxxer]\ncommand = %q\nargs = [%s]\n",
		cmdPath, strings.Join(args, ", "))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return true, os.WriteFile(path, []byte(out+block), 0644)
}

// appendMarkedBlock appends a marker-delimited block if the marker is absent;
// an existing block (possibly hand-edited) is left untouched.
func appendMarkedBlock(path, startMark, endMark, body string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if strings.Contains(string(existing), startMark) {
		return false, nil
	}
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	block := startMark + "\n" + body + endMark + "\n"
	if out != "" {
		block = "\n" + block
	}
	return true, os.WriteFile(path, []byte(out+block), 0644)
}

// --- shared JSON helpers ---

func loadJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("existing file is not valid JSON (fix or remove it first): %w", err)
	}
	return doc, nil
}

func saveJSONObject(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func writeFileIfDifferent(path, content string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == content {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), 0644)
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}
