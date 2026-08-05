package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// RunInit is the one-shot onboarding command: it indexes a repo, stages the
// host-agent enrich flow, and WRITES the host configs (MCP registration +
// adoption hook/rule) for the requested agent hosts. DECISION(2026-07): init
// used to only print paste-me blocks; the beta flow is now "tell your agent
// to install this", so the deterministic merge lives here where it is tested,
// instead of every host agent free-styling JSON edits. -no-write restores the
// print-only behavior; every writer merges and is idempotent (see hostsetup.go).
func RunInit(ctx context.Context, args []string, log *slog.Logger, noEmbeddings, includeTests bool, modelName string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	host := fs.String("host", HostAuto, "agent host to configure: claude-code|cursor|codex|auto (auto = every host detected)")
	noWrite := fs.Bool("no-write", false, "print config blocks instead of writing host files")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := "."
	if fs.NArg() > 0 {
		root = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}

	fmt.Printf("==> Indexing %s\n", absRoot)
	if err := RunIndex(ctx, []string{root}, log, "", noEmbeddings, includeTests, modelName, false); err != nil {
		return fmt.Errorf("index: %w", err)
	}

	fmt.Printf("\n==> Staging enrich (host-agent flow)\n")
	if err := RunEnrich(ctx, []string{"-emit-pending", root}, log, modelName); err != nil {
		return fmt.Errorf("enrich: %w", err)
	}

	if added, err := EnsureGitignore(absRoot); err != nil {
		log.Warn("update .gitignore failed (add .contextmaxxer/ yourself)", "err", err)
	} else if added {
		fmt.Printf("\n==> Added .contextmaxxer/ to .gitignore (index artifacts are binary and multi-MB)\n")
	}

	dbPath := filepath.Join(absRoot, ".contextmaxxer", "index.db")
	cmdPath, err := os.Executable()
	if err != nil || cmdPath == "" {
		cmdPath = "contextmaxxer"
	}

	if *noWrite {
		printConfigBlocks(absRoot, cmdPath, dbPath, root)
		return nil
	}

	hosts := []string{*host}
	if *host == HostAuto {
		hosts = DetectHosts(absRoot)
		if len(hosts) == 0 {
			fmt.Printf("\n==> No agent host detected (.claude/.cursor/~/.codex) — printing config blocks instead.\n")
			printConfigBlocks(absRoot, cmdPath, dbPath, root)
			return nil
		}
	}

	fmt.Printf("\n==> Writing host configs (%s)\n", strings.Join(hosts, ", "))
	var touched []string
	for _, h := range hosts {
		files, err := WriteHostConfig(h, absRoot, cmdPath, dbPath)
		if err != nil {
			return fmt.Errorf("host setup: %w", err)
		}
		touched = append(touched, files...)
	}
	if len(touched) == 0 {
		fmt.Println("  Everything was already configured — nothing to change.")
	}
	for _, f := range touched {
		fmt.Printf("  wrote %s\n", f)
	}

	fmt.Printf(`
==> Next steps
  1. Restart the agent and approve the "contextmaxxer" MCP server when asked
     (only a human can do this part).
  2. (Recommended) Generate purpose summaries — the single biggest quality
     lever. Tell your agent to process .contextmaxxer/pending-docs.json into
     .contextmaxxer/synthetic-docs.json (see BETA.md step 3), then run:
       %s -force index %s
  3. Ask the agent a question — it now leads with find_context.
`, cmdPath, root)
	return nil
}

// printConfigBlocks is the -no-write fallback: ready-to-paste blocks with all
// paths resolved.
func printConfigBlocks(absRoot, cmdPath, dbPath, root string) {
	fmt.Printf("\n==> MCP config — paste into %s:\n", agentConfigHint(absRoot))
	fmt.Printf(`
{
  "mcpServers": {
    "contextmaxxer": {
      "command": %q,
      "args": ["mcp", "--index", %q, "--reranker", "jina-reranker-v1-tiny-en", "--adaptive-rerank", "--watch"]
    }
  }
}
`, cmdPath, dbPath)

	fmt.Printf("\n==> Adoption — make the agent actually reach for find_context (see BETA.md step 4)\n")
	fmt.Printf("  Claude Code: add to .claude/settings.json — a hook that gates grep until find_context is called:\n")
	fmt.Printf(`
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "Grep|Glob", "hooks": [{ "type": "command", "command": %q }] }
    ],
    "PostToolUse": [
      { "matcher": "mcp__.*__find_context", "hooks": [{ "type": "command", "command": %q }] }
    ]
  }
}
`, cmdPath+" hook pre-search", cmdPath+" hook post-find")
	fmt.Printf("  Cursor (no hooks): add a standing rule — copy the .cursor/rules/contextmaxxer.mdc block from BETA.md step 4.\n")
	fmt.Printf("  Codex: add [mcp_servers.contextmaxxer] to ~/.codex/config.toml and the rule to AGENTS.md (or rerun init without -no-write).\n")

	fmt.Printf(`
==> Next steps
  1. (Recommended) Generate purpose summaries with your coding agent, then
     re-index so they reach the embeddings:
       run the /enrich-docs flow, or fill .contextmaxxer/synthetic-docs.json
       then: %s -force index %s
  2. Paste the MCP block into your agent config; add the adoption hook/rule above.
  3. Restart the agent, then ask it a question — it now leads with find_context.
`, cmdPath, root)
}

// agentConfigHint guesses the right MCP config path from which agent dir exists
// in the repo, falling back to naming both. Claude Code reads project-scoped MCP
// servers from .mcp.json at the repo root (NOT .claude/mcp.json); Cursor uses
// .cursor/mcp.json.
func agentConfigHint(absRoot string) string {
	_, claudeErr := os.Stat(filepath.Join(absRoot, ".claude"))
	_, cursorErr := os.Stat(filepath.Join(absRoot, ".cursor"))
	switch {
	case claudeErr == nil && cursorErr != nil:
		return ".mcp.json"
	case cursorErr == nil && claudeErr != nil:
		return ".cursor/mcp.json"
	default:
		return ".mcp.json (Claude Code) or .cursor/mcp.json (Cursor)"
	}
}
