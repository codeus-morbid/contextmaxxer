package main

import (
	"regexp"
	"strings"
)

// Query shaping. The benchmark hands us a raw issue report — title, prose,
// reproduction steps, expected/current behaviour, often a stack trace. The
// NodeBB instances run about 850 characters of it.
//
// DECISION(2026-09): this is the one stage worth attacking. Every ablation left
// HitFile at 0.531 — graph, PageRank, cross-encoder, intent ranker, a 10x seed
// pool, even a fine-tuned embedder — so the ceiling is not in ranking, it is in
// what we search FOR. A whole issue report averages into a vague vector and
// fills the lexical channel with "Steps to Reproduce" and "Expected behavior".
// Precedent: agent-reformulated queries lifted Hit@1 0.70 -> 0.83 on the gen
// corpus, a larger move than anything measured since.
const (
	queryRaw    = "raw"
	queryTitle  = "title"
	queryIDs    = "ids"
	queryTitleI = "title+ids"
)

// codeIdentifier matches the shapes an issue uses to name code: backticked
// spans, CamelCase, snake_case, dotted paths and file paths.
var (
	reBackticked = regexp.MustCompile("`([^`\n]{2,80})`")
	reCamel      = regexp.MustCompile(`\b[a-z]+[A-Z][A-Za-z0-9]*\b|\b[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]*\b`)
	reSnake      = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
	rePath       = regexp.MustCompile(`\b[\w.-]+(?:/[\w.-]+)+\.\w{1,5}\b`)
	reDotted     = regexp.MustCompile(`\b[a-z][\w]*(?:\.[a-z][\w]*){1,4}\b`)
	// Markdown scaffolding the benchmark's issues are wrapped in.
	reHeading = regexp.MustCompile(`(?m)^#{1,6}\s*`)
)

// shapeQuery rewrites the issue text for the requested mode.
func shapeQuery(text, mode string) string {
	switch mode {
	case queryTitle:
		return issueTitle(text)
	case queryIDs:
		return strings.Join(codeIdentifiers(text, 24), " ")
	case queryTitleI:
		title := issueTitle(text)
		ids := codeIdentifiers(text, 16)
		if len(ids) == 0 {
			return title
		}
		return title + " " + strings.Join(ids, " ")
	default:
		return text
	}
}

// issueTitle returns the first line that carries actual words. Reports here
// commonly open with a "## Title:" heading whose text is on a later line, so
// the heading marker alone is not the title.
func issueTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(reHeading.ReplaceAllString(line, ""))
		line = strings.TrimSuffix(strings.TrimSpace(line), ":")
		if line == "" {
			continue
		}
		// Skip the scaffolding words that are headings rather than content.
		switch strings.ToLower(line) {
		case "title", "description", "summary", "steps to reproduce",
			"step to reproduce", "expected behavior", "expected behaviour",
			"current behavior", "current behaviour", "actual behavior":
			continue
		}
		if strings.HasPrefix(line, "<!--") || strings.HasPrefix(line, "```") {
			continue
		}
		return line
	}
	return strings.TrimSpace(text)
}

// codeIdentifiers pulls the names an issue uses for code, most distinctive
// first: backticked spans are the author's own emphasis, so they lead.
func codeIdentifiers(text string, limit int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.Trim(s, "`.,;:()[]{}\"'")
		if len(s) < 3 || len(s) > 80 || seen[strings.ToLower(s)] {
			return
		}
		if isStopWord(s) {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	for _, m := range reBackticked.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, re := range []*regexp.Regexp{rePath, reSnake, reCamel, reDotted} {
		for _, m := range re.FindAllString(text, -1) {
			add(m)
		}
		if len(out) >= limit {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// isStopWord drops prose that merely looks like an identifier. Without this the
// dotted-path pattern turns every sentence ending into a "path".
func isStopWord(s string) bool {
	switch strings.ToLower(s) {
	case "e.g", "i.e", "etc", "vs", "self", "true", "false", "none", "null",
		"the", "and", "for", "with", "this", "that", "when", "then", "should",
		"github.com", "readme.md":
		return true
	}
	return false
}
