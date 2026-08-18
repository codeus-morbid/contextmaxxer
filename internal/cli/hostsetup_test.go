package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid json in %s: %v", path, err)
	}
	return doc
}

func TestMergeMCPServersPreservesOthersAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	seed := `{"mcpServers":{"other":{"command":"foo","args":["bar"]}},"unrelated":42}`
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := mergeMCPServers(path, "C:\\bin\\ctxm.exe", "C:\\repo\\.contextmaxxer\\index.db")
	if err != nil || !changed {
		t.Fatalf("first merge: changed=%v err=%v", changed, err)
	}
	doc := readJSON(t, path)
	servers := doc["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatal("pre-existing server was dropped")
	}
	if _, ok := servers["contextmaxxer"]; !ok {
		t.Fatal("contextmaxxer server not added")
	}
	if doc["unrelated"].(float64) != 42 {
		t.Fatal("unrelated top-level key lost")
	}

	changed, err = mergeMCPServers(path, "C:\\bin\\ctxm.exe", "C:\\repo\\.contextmaxxer\\index.db")
	if err != nil || changed {
		t.Fatalf("second merge must be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestMergeMCPServersRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	if err := os.WriteFile(path, []byte("{broken"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeMCPServers(path, "x", "y"); err == nil {
		t.Fatal("expected error on invalid existing JSON, got nil (silent clobber risk)")
	}
}

func TestMergeClaudeHooksIdempotentAndPreserving(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	seed := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"my-own-hook"}]}]},"permissions":{"allow":["Bash(go test:*)"]}}`
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := mergeClaudeHooks(path, "ctxm")
	if err != nil || !changed {
		t.Fatalf("first merge: changed=%v err=%v", changed, err)
	}
	doc := readJSON(t, path)
	if _, ok := doc["permissions"]; !ok {
		t.Fatal("permissions key lost")
	}
	pre := doc["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("want user hook + gate hook, got %d entries", len(pre))
	}
	post := doc["hooks"].(map[string]any)["PostToolUse"].([]any)
	if len(post) != 1 {
		t.Fatalf("want 1 PostToolUse entry, got %d", len(post))
	}

	changed, err = mergeClaudeHooks(path, "ctxm")
	if err != nil || changed {
		t.Fatalf("second merge must be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestAppendCodexServerIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	seed := "model = \"o4\"\n\n[mcp_servers.other]\ncommand = \"foo\"\n"
	if err := os.WriteFile(path, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := appendCodexServer(path, "/usr/local/bin/ctxm", "/repo/.contextmaxxer/index.db")
	if err != nil || !changed {
		t.Fatalf("first append: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "model = \"o4\"") || !strings.Contains(string(data), "[mcp_servers.other]") {
		t.Fatal("existing toml content lost")
	}
	if !strings.Contains(string(data), "[mcp_servers.contextmaxxer]") {
		t.Fatal("contextmaxxer section not appended")
	}

	changed, err = appendCodexServer(path, "/usr/local/bin/ctxm", "/repo/.contextmaxxer/index.db")
	if err != nil || changed {
		t.Fatalf("second append must be a no-op: changed=%v err=%v", changed, err)
	}

	changed, err = appendCodexServer(path, "/new/ctxm", "/new/repo/index.db")
	if err != nil || !changed {
		t.Fatalf("stale managed keys must update: changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(path)
	updated := string(data)
	if !strings.Contains(updated, `command = "/new/ctxm"`) || !strings.Contains(updated, `"/new/repo/index.db"`) {
		t.Fatal("managed command/args were not updated")
	}
	if !strings.Contains(updated, "[mcp_servers.other]") {
		t.Fatal("unrelated section lost during managed-key update")
	}
	changed, err = appendCodexServer(path, "/new/ctxm", "/new/repo/index.db")
	if err != nil || changed {
		t.Fatalf("updated section must become idempotent: changed=%v err=%v", changed, err)
	}
}

func TestAppendMarkedBlockIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("# My agents file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	changed, err := appendMarkedBlock(path, "<!-- s -->", "<!-- e -->", "body\n")
	if err != nil || !changed {
		t.Fatalf("first append: changed=%v err=%v", changed, err)
	}
	changed, err = appendMarkedBlock(path, "<!-- s -->", "<!-- e -->", "body\n")
	if err != nil || changed {
		t.Fatalf("second append must be a no-op: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "# My agents file") {
		t.Fatal("existing content lost")
	}

	changed, err = appendMarkedBlock(path, "<!-- s -->", "<!-- e -->", "new body\n")
	if err != nil || !changed {
		t.Fatalf("managed block update: changed=%v err=%v", changed, err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- s -->\nnew body\n<!-- e -->") || strings.Contains(string(data), "\nbody\n") {
		t.Fatal("managed block content was not replaced")
	}
	if !strings.HasPrefix(string(data), "# My agents file") {
		t.Fatal("content outside managed block lost")
	}
}

func TestAppendMarkedBlockRejectsUnclosedManagedBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, []byte("before\n<!-- s -->\nstale\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := appendMarkedBlock(path, "<!-- s -->", "<!-- e -->", "body\n")
	if err == nil || !strings.Contains(err.Error(), "without") {
		t.Fatalf("want unclosed-block error, got %v", err)
	}
}

func TestAdoptionRuleUsesExactExpansionForMissingBodies(t *testing.T) {
	for _, fragment := range []string{
		"Leave tuning knobs",
		"call expand_context",
		"prove its branch, feature flag, protocol, or dispatch discriminator",
		"continue_context",
		"until status:complete",
		"instead of repeating semantic search",
	} {
		if !strings.Contains(adoptionRule, fragment) {
			t.Fatalf("adoption rule missing %q", fragment)
		}
	}
}

func TestWriteCursorFiles(t *testing.T) {
	dir := t.TempDir()
	touched, err := writeCursor(dir, "ctxm", filepath.Join(dir, ".contextmaxxer", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(touched) != 2 {
		t.Fatalf("want 2 touched files, got %v", touched)
	}
	rule, err := os.ReadFile(filepath.Join(dir, ".cursor", "rules", "contextmaxxer.mdc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rule), "alwaysApply: true") {
		t.Fatal("rule frontmatter missing")
	}

	touched, err = writeCursor(dir, "ctxm", filepath.Join(dir, ".contextmaxxer", "index.db"))
	if err != nil || len(touched) != 0 {
		t.Fatalf("second run must touch nothing: %v err=%v", touched, err)
	}
}
