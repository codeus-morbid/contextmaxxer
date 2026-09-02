package index

import (
	"strings"
	"unicode"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

const (
	embedDocstringMax = 500
	embedSignatureMax = 300
	embedBodyMax      = 1600
)

// EmbeddingText builds the index-time text embedded for a symbol.
// DECISION(2026-06): v2 profile — a natural-language line (name words, kind,
// path words) leads the text, followed by docstring, so paraphrastic queries
// can match symbols that have no doc comment. ASSUMES: the embedder weights
// early tokens more and the symbol name carries the core semantics.
// REVISIT IF: paraphrastic slice on the gen corpus does not improve vs v1.
func EmbeddingText(filePath, language string, sym store.Symbol) string {
	return EmbeddingTextWithNeighbors(filePath, language, sym, nil)
}

// embedNeighborMax caps how many graph neighbours join the text. A few names
// add vocabulary; a long list would drown the symbol's own words, and hub
// symbols have hundreds of neighbours.
const embedNeighborMax = 8

// EmbeddingTextWithNeighbors adds the names of the symbol's graph neighbours.
//
// DECISION(2026-09): the callers and callees of a symbol are free vocabulary
// that the symbol's own text does not carry. `resize()` in image.js has nothing
// to do with the word "avatar" until you notice that `uploadAvatar` calls it —
// which is exactly the gap that makes an issue report fail to find the code it
// describes. Measured on SWE-Explore, 14.3% of gold files sit in the index and
// are never retrieved even at rank 200, and every attempt to reach them by
// ranking, pool size or graph traversal failed; this attacks the same gap from
// the indexing side, at no query cost and with no model.
//
// Only the backfill path can supply neighbours, because during the first index
// pass the edges do not exist yet — embeddings are computed before the call
// graph is extracted.
// ASSUMES: neighbour names are more signal than noise at this cap.
// REVISIT IF: hub symbols (high degree) get worse rather than better.
func EmbeddingTextWithNeighbors(filePath, language string, sym store.Symbol, neighbors []string) string {
	var b strings.Builder

	if head := identifierWords(sym.QualifiedName); head != "" {
		b.WriteString(head)
		b.WriteString(" - ")
	}
	b.WriteString(sym.Kind)
	if pw := identifierWords(filePath); pw != "" {
		b.WriteString(" in ")
		b.WriteString(pw)
	}
	b.WriteString("\n")

	if sym.Docstring != "" {
		b.WriteString(truncateField(sym.Docstring, embedDocstringMax))
		b.WriteString("\n")
	}

	// Neighbour names ride in the natural-language head, before the structured
	// fields, because the profile above assumes the embedder weights early
	// tokens most.
	if len(neighbors) > 0 {
		var words []string
		for _, n := range neighbors {
			if len(words) >= embedNeighborMax {
				break
			}
			if w := identifierWords(n); w != "" {
				words = append(words, w)
			}
		}
		if len(words) > 0 {
			b.WriteString("related: ")
			b.WriteString(strings.Join(words, ", "))
			b.WriteString("\n")
		}
	}

	writeField(&b, "symbol", sym.QualifiedName)
	writeField(&b, "kind", sym.Kind)
	writeField(&b, "file", filePath)
	writeField(&b, "language", language)
	writeField(&b, "signature", truncateField(sym.Signature, embedSignatureMax))
	if sym.BodyExcerpt != "" {
		b.WriteString("body:\n")
		b.WriteString(truncateField(sym.BodyExcerpt, embedBodyMax))
	}
	return b.String()
}

// identifierWords renders an identifier or path as lowercase natural-language
// words: "retrieve.effectiveAlpha" -> "retrieve effective alpha",
// "internal/rerank/onnx.go" -> "internal rerank onnx go".
func identifierWords(s string) string {
	if s == "" {
		return ""
	}
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur.WriteRune(r)
	}
	flush()
	return strings.Join(words, " ")
}

func truncateField(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + " ..."
}

func writeField(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteString("\n")
}
