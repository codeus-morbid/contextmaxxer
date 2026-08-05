package index

import (
	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// LanguageExtractor extracts symbols and edges from a parsed syntax tree.
// SymbolID/FileID are NOT set on returned symbols — caller assigns after persistence.
// DECISION: intra-file resolution only — cross-file is fuzzy match later.
type LanguageExtractor interface {
	Symbols(tree *tree_sitter.Tree, source []byte) []store.Symbol
	Edges(tree *tree_sitter.Tree, source []byte, nameToID map[string]int64) []store.Edge
}
