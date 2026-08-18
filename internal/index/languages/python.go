package languages

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type pyExtractor struct{}

func pyLanguage() *tree_sitter.Language {
	return tree_sitter.NewLanguage(tree_sitter_python.Language())
}

func NewPyParser() *tree_sitter.Parser {
	p := tree_sitter.NewParser()
	p.SetLanguage(pyLanguage())
	return p
}

func (e *pyExtractor) Symbols(tree *tree_sitter.Tree, source []byte) []store.Symbol {
	root := tree.RootNode()
	var symbols []store.Symbol
	symbols = pyWalkBlock(root, source, "", symbols)
	return symbols
}

func pyWalkBlock(node *tree_sitter.Node, source []byte, className string, symbols []store.Symbol) []store.Symbol {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "function_definition":
			kind := store.KindFunction
			if className != "" {
				kind = store.KindMethod
			}
			sym := pyFuncSymbol(child, source, className, kind)
			symbols = append(symbols, sym)
		case "class_definition":
			sym := pyClassSymbol(child, source)
			symbols = append(symbols, sym)
			body := child.ChildByFieldName("body")
			if body != nil {
				symbols = pyWalkBlock(body, source, sym.Name, symbols)
			}
		case "decorated_definition":
			// unwrap decorator — the actual definition is the last child
			inner := child.Child(child.ChildCount() - 1)
			if inner == nil {
				continue
			}
			switch inner.Kind() {
			case "function_definition":
				kind := store.KindFunction
				if className != "" {
					kind = store.KindMethod
				}
				sym := pyFuncSymbol(inner, source, className, kind)
				symbols = append(symbols, sym)
			case "class_definition":
				sym := pyClassSymbol(inner, source)
				symbols = append(symbols, sym)
				body := inner.ChildByFieldName("body")
				if body != nil {
					symbols = pyWalkBlock(body, source, sym.Name, symbols)
				}
			}
		}
	}
	return symbols
}

func pyFuncSymbol(node *tree_sitter.Node, source []byte, className string, kind string) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	qname := name
	if className != "" {
		qname = className + "." + name
	}
	doc := pyDocstring(node, source)
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          kind,
		QualifiedName: qname,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	}
}

func pyClassSymbol(node *tree_sitter.Node, source []byte) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	doc := pyDocstring(node, source)
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindClass,
		QualifiedName: name,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	}
}

// pyDocstring extracts the first string literal in the body block.
func pyDocstring(node *tree_sitter.Node, source []byte) string {
	body := node.ChildByFieldName("body")
	if body == nil {
		return ""
	}
	for i := range body.ChildCount() {
		child := body.Child(i)
		if child == nil {
			continue
		}
		if child.Kind() == "expression_statement" {
			for j := range child.ChildCount() {
				c := child.Child(j)
				if c != nil && c.Kind() == "string" {
					return nodeText(c, source)
				}
			}
		}
		break
	}
	return ""
}

func (e *pyExtractor) Edges(tree *tree_sitter.Tree, source []byte, nameToID map[string]int64) []store.Edge {
	if len(nameToID) == 0 {
		return nil
	}

	root := tree.RootNode()
	lang := pyLanguage()

	// DECISION: intra-file resolution only — cross-file is fuzzy match later.
	q, qErr := tree_sitter.NewQuery(lang,
		`(call function: [(identifier) @fn (attribute attribute: (identifier) @fn)])`)
	if qErr != nil {
		return nil
	}
	defer q.Close()

	var edges []store.Edge
	edges = pyExtractEdgesFromBlock(root, source, q, nameToID, edges, "")
	return edges
}

func pyExtractEdgesFromBlock(node *tree_sitter.Node, source []byte, q *tree_sitter.Query, nameToID map[string]int64, edges []store.Edge, className string) []store.Edge {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "function_definition":
			edges = pyExtractCallsInFunc(child, source, q, nameToID, edges, className)
		case "class_definition":
			name := nodeText(child.ChildByFieldName("name"), source)
			body := child.ChildByFieldName("body")
			if body != nil {
				edges = pyExtractEdgesFromBlock(body, source, q, nameToID, edges, name)
			}
		case "decorated_definition":
			inner := child.Child(child.ChildCount() - 1)
			if inner == nil {
				continue
			}
			switch inner.Kind() {
			case "function_definition":
				edges = pyExtractCallsInFunc(inner, source, q, nameToID, edges, className)
			case "class_definition":
				name := nodeText(inner.ChildByFieldName("name"), source)
				body := inner.ChildByFieldName("body")
				if body != nil {
					edges = pyExtractEdgesFromBlock(body, source, q, nameToID, edges, name)
				}
			}
		}
	}
	return edges
}

func pyExtractCallsInFunc(node *tree_sitter.Node, source []byte, q *tree_sitter.Query, nameToID map[string]int64, edges []store.Edge, className string) []store.Edge {
	name := nodeText(node.ChildByFieldName("name"), source)
	srcQName := name
	if className != "" {
		srcQName = className + "." + name
	}
	srcID, ok := nameToID[srcQName]
	if !ok {
		return edges
	}

	body := node.ChildByFieldName("body")
	if body == nil {
		return edges
	}

	cursor := tree_sitter.NewQueryCursor()
	defer cursor.Close()
	matches := cursor.Matches(q, body, source)
	seen := map[string]bool{}
	for {
		m := matches.Next()
		if m == nil {
			break
		}
		for _, cap := range m.Captures {
			callee := nodeText(&cap.Node, source)
			callLine := int(cap.Node.StartPosition().Row) + 1
			if seen[callee] {
				continue
			}
			seen[callee] = true
			if dstID, found := nameToID[callee]; found && dstID != srcID {
				edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
				continue
			}
			// DECISION(2026-06): resolve a dotted callee by UNIQUE suffix only.
			// Emitting an edge to every same-named symbol exploded the Django
			// graph to ~35 edges/symbol (hundreds of save/get/clean/__init__).
			// Mirror the Go extractor: an ambiguous suffix (>1 match) is no edge.
			var matchID int64
			count := 0
			for qn, dstID := range nameToID {
				if dstID == srcID {
					continue
				}
				parts := strings.Split(qn, ".")
				if parts[len(parts)-1] == callee {
					matchID = dstID
					count++
					if count > 1 {
						break
					}
				}
			}
			if count == 1 {
				edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: matchID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
			}
		}
	}
	return edges
}
