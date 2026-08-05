package index

import (
	"regexp"
	"strings"
)

// NormalizeDocstring keeps the prose of a doc comment and drops the machinery
// around it: annotation-only lines, tool directives and markup noise.
//
// DECISION(2026-08): a docstring is not free text to us — it is the largest
// semantic field in the embedding text, the reranker document and the agent
// payload. In three ecosystems the MAJORITY of what extractors captured
// carried no meaning at all: PHP 70% of documented symbols had only
// `@param Type $x` lines, C++ 59% (plus 22 symbols whose entire "doc" was a
// NOLINT suppression), TypeScript 61% were bare `/** @internal */`. Embedding
// those shapes the vector by annotation syntax instead of purpose, and a
// symbol left with nothing is better served by its name, signature and body.
// REVISIT IF: a language arrives whose tags carry the only description
// (then extract the tag's text rather than dropping the line).
func NormalizeDocstring(doc string) string {
	if doc == "" {
		return ""
	}
	var kept []string
	for _, raw := range strings.Split(doc, "\n") {
		line := strings.TrimSpace(raw)
		// Strip comment furniture the extractors leave in place.
		line = strings.TrimPrefix(line, "/**")
		line = strings.TrimPrefix(line, "/*")
		line = strings.TrimSuffix(line, "*/")
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "///")
		line = strings.TrimPrefix(line, "//!")
		line = strings.TrimPrefix(line, "//")
		line = strings.TrimPrefix(line, "*")
		line = strings.TrimPrefix(line, "!")
		line = strings.TrimPrefix(line, "#")
		line = strings.TrimSpace(line)
		if line == "" || isDocNoise(line) {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.TrimSpace(strings.Join(kept, "\n"))
	// A leftover fragment of punctuation or a lone identifier is not prose.
	if len(strings.Fields(out)) < 2 {
		return ""
	}
	return out
}

// docTag matches a line that begins with a documentation tag in any of the
// conventions we index (javadoc/jsdoc/phpdoc `@x`, doxygen `\x`).
var docTag = regexp.MustCompile(`^[@\\][a-zA-Z]+`)

// toolDirective matches linter/compiler pragmas that live in comments and
// describe the toolchain, never the code's purpose.
var toolDirective = regexp.MustCompile(`^(NOLINT|nolint|noinspection|eslint-|prettier-|tslint:|@ts-|istanbul |c8 |codebeat|SPDX-|Copyright|coverage:)`)

func isDocNoise(line string) bool {
	if docTag.MatchString(line) || toolDirective.MatchString(line) {
		return true
	}
	// Markup-only leftovers ("{{{", "}}}", "---", "===", table rules).
	trimmed := strings.Trim(line, "-=~_*{}[]()<>|/\\ \t")
	return trimmed == ""
}
