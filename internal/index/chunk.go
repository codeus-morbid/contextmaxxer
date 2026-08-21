package index

import (
	"os"
	"strconv"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// Chunking exists because every retrieval channel reads body_excerpt, which the
// extractor caps: for a capped symbol the code past the cap has no vector at
// all. Only capped symbols are chunked — everything else is already fully
// represented by its own embedding, and chunking it would double the index for
// nothing.
const (
	// chunkLines is sized to match embedBodyMax (1600 bytes) at Go's typical
	// line length, so a chunk carries about as much code as the per-symbol
	// embedding does.
	chunkLines = 40
	// chunkOverlap keeps a statement that straddles a boundary whole in one of
	// the two chunks.
	chunkOverlap = 10
	// chunkMaxPerSymbol bounds a pathological generated function.
	chunkMaxPerSymbol = 48
	// chunkMaxBytes keeps one window of very long lines from shipping megabytes
	// to the embedder, which truncates by tokens anyway.
	chunkMaxBytes = 4000
)

// ChunkSymbol splits a capped symbol's lossless body into embeddable windows.
// Each chunk is prefixed with the same natural-language header the per-symbol
// embedding leads with, because a bare window of code carries no clue about
// which symbol it belongs to. Returns nil when the symbol is not capped.
func ChunkSymbol(filePath, language string, sym store.Symbol) []store.SymbolChunk {
	if sym.FullBody == "" {
		return nil
	}
	lines := strings.Split(sym.FullBody, "\n")
	if len(lines) < 2 {
		// One line, however long: there is no window to cut. The embedder would
		// truncate it exactly where the excerpt already does.
		return nil
	}
	// Note the absence of a "too few lines to bother" guard: a capped body of
	// forty long lines still hides code past the cap, and skipping it would
	// leave precisely the symbols this exists for unrepresented.
	header := chunkHeader(filePath, sym)

	stride := chunkLines - chunkOverlap
	var out []store.SymbolChunk
	for start := 0; start < len(lines); start += stride {
		end := start + chunkLines
		if end > len(lines) {
			end = len(lines)
		}
		body := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		if len(body) > chunkMaxBytes {
			body = truncateField(body, chunkMaxBytes)
		}
		if body != "" {
			out = append(out, store.SymbolChunk{
				SymbolID:  sym.ID,
				StartLine: sym.StartLine + start,
				Text:      header + body,
			})
		}
		if end == len(lines) || len(out) == chunkMaxPerSymbol {
			break
		}
	}
	return out
}

func chunkHeader(filePath string, sym store.Symbol) string {
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
	return b.String()
}

// ChunkingEnabled reports whether chunk embeddings should be built. It reads
// the same switch the retrieval channel uses, so the two cannot disagree: an
// index full of chunks no query will ever read is pure waste, and a weight
// pointing at chunks that were never built is a silent no-op.
func ChunkingEnabled() bool {
	v := os.Getenv("CONTEXTMAXXER_CHUNK_VEC_WEIGHT")
	if v == "" {
		return false
	}
	f, err := strconv.ParseFloat(v, 32)
	return err == nil && f > 0
}
