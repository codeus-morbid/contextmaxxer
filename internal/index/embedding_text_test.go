package index

import (
	"fmt"
	"strings"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// Graph neighbours give a symbol vocabulary its own text lacks. Worked out by
// hand: resize() has nothing to do with "avatar" until uploadAvatar, which
// calls it, is named alongside — and identifier words are split, so the text
// carries "upload avatar".
func TestEmbeddingTextWithNeighbors_AddsNeighbourVocabulary(t *testing.T) {
	sym := store.Symbol{QualifiedName: "image.resize", Kind: "function", Signature: "func resize(w, h int)"}

	plain := EmbeddingText("src/image.js", "javascript", sym)
	if strings.Contains(plain, "avatar") {
		t.Fatal("the symbol alone must not mention avatar")
	}

	withN := EmbeddingTextWithNeighbors("src/image.js", "javascript", sym,
		[]string{"upload.uploadAvatar", "math.clamp"})
	for _, want := range []string{"related:", "upload avatar", "clamp"} {
		if !strings.Contains(withN, want) {
			t.Fatalf("missing %q in:\n%s", want, withN)
		}
	}
}

// A hub symbol must not drown in its own neighbours.
func TestEmbeddingTextWithNeighbors_CapsTheList(t *testing.T) {
	var many []string
	for i := 0; i < 50; i++ {
		many = append(many, fmt.Sprintf("pkg.neighbour%d", i))
	}
	got := EmbeddingTextWithNeighbors("a.go", "go",
		store.Symbol{QualifiedName: "pkg.hub", Kind: "function"}, many)

	if n := strings.Count(got, "neighbour"); n > embedNeighborMax {
		t.Fatalf("expected at most %d neighbours, got %d", embedNeighborMax, n)
	}
}

// Without neighbours the text is exactly what it always was, so the first
// index pass (where edges do not exist yet) is unaffected.
func TestEmbeddingTextWithNeighbors_NilIsUnchanged(t *testing.T) {
	sym := store.Symbol{QualifiedName: "pkg.f", Kind: "function"}
	if EmbeddingTextWithNeighbors("a.go", "go", sym, nil) != EmbeddingText("a.go", "go", sym) {
		t.Fatal("nil neighbours must produce the original text")
	}
}
