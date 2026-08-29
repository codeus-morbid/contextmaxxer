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

	bases := pyClassBases(root, source, map[string][]string{})

	var edges []store.Edge
	edges = pyExtractEdgesFromBlock(root, source, q, nameToID, bases, edges, "")
	return edges
}

// pyClassBases maps each class in this file to the last name segment of every
// base it declares, so `self.m()` can be followed up the inheritance chain.
// A base defined in another file still resolves — the class name alone keys the
// global qname map — but its OWN bases are out of reach here, so the walk stops
// at the first hop that leaves the file.
func pyClassBases(node *tree_sitter.Node, source []byte, out map[string][]string) map[string][]string {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil {
			continue
		}
		if child.Kind() == "decorated_definition" {
			if inner := child.Child(child.ChildCount() - 1); inner != nil {
				child = inner
			}
		}
		if child.Kind() != "class_definition" {
			if body := child.ChildByFieldName("body"); body != nil {
				pyClassBases(body, source, out)
			}
			continue
		}
		name := nodeText(child.ChildByFieldName("name"), source)
		if supers := child.ChildByFieldName("superclasses"); supers != nil && name != "" {
			for j := range supers.ChildCount() {
				arg := supers.Child(j)
				if arg == nil {
					continue
				}
				switch arg.Kind() {
				case "identifier":
					out[name] = append(out[name], nodeText(arg, source))
				case "attribute":
					// `base.BaseHandler` — the module prefix is not part of a qname.
					out[name] = append(out[name], nodeText(arg.ChildByFieldName("attribute"), source))
				}
			}
		}
		if body := child.ChildByFieldName("body"); body != nil {
			pyClassBases(body, source, out)
		}
	}
	return out
}

// pyResolveSelfCall binds `self.callee()` to the enclosing class, then to its
// bases in declaration order. Breadth-first, so a method the class overrides
// wins over the inherited one.
func pyResolveSelfCall(className, callee string, bases map[string][]string, nameToID map[string]int64, srcID int64) (int64, bool) {
	seen := map[string]bool{}
	queue := []string{className}
	for len(queue) > 0 {
		cls := queue[0]
		queue = queue[1:]
		if cls == "" || seen[cls] {
			continue
		}
		seen[cls] = true
		if id, ok := nameToID[cls+"."+callee]; ok && id != srcID {
			return id, true
		}
		queue = append(queue, bases[cls]...)
	}
	return 0, false
}

func pyExtractEdgesFromBlock(node *tree_sitter.Node, source []byte, q *tree_sitter.Query, nameToID map[string]int64, bases map[string][]string, edges []store.Edge, className string) []store.Edge {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "function_definition":
			edges = pyExtractCallsInFunc(child, source, q, nameToID, bases, edges, className)
		case "class_definition":
			name := nodeText(child.ChildByFieldName("name"), source)
			body := child.ChildByFieldName("body")
			if body != nil {
				edges = pyExtractEdgesFromBlock(body, source, q, nameToID, bases, edges, name)
			}
		case "decorated_definition":
			inner := child.Child(child.ChildCount() - 1)
			if inner == nil {
				continue
			}
			switch inner.Kind() {
			case "function_definition":
				edges = pyExtractCallsInFunc(inner, source, q, nameToID, bases, edges, className)
			case "class_definition":
				name := nodeText(inner.ChildByFieldName("name"), source)
				body := inner.ChildByFieldName("body")
				if body != nil {
					edges = pyExtractEdgesFromBlock(body, source, q, nameToID, bases, edges, name)
				}
			}
		}
	}
	return edges
}

func pyExtractCallsInFunc(node *tree_sitter.Node, source []byte, q *tree_sitter.Query, nameToID map[string]int64, bases map[string][]string, edges []store.Edge, className string) []store.Edge {
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

			// The query captures only the attribute; the receiver is its sibling.
			receiver := ""
			if parent := cap.Node.Parent(); parent != nil && parent.Kind() == "attribute" {
				receiver = nodeText(parent.ChildByFieldName("object"), source)
			}
			// Two calls of the same name on different receivers are two edges.
			seenKey := receiver + "|" + callee
			if seen[seenKey] {
				continue
			}
			seen[seenKey] = true

			// DECISION(2026-08): resolve `self.m()` against the enclosing class and
			// its bases before the global fallback. Go and TypeScript already bind a
			// known receiver to its type; Python did not, so the unique-suffix guard
			// below silently dropped the single most common call form in the
			// language — Django's `self.clean()` and `self._fetch_all()` produced
			// ZERO edges because the name repeats across classes.
			// ASSUMES: `self`/`cls` is the instance, not rebound. REVISIT IF: metaclass
			// or mixin-heavy code needs the full MRO instead of declared bases.
			if (receiver == "self" || receiver == "cls") && className != "" {
				if dstID, found := pyResolveSelfCall(className, callee, bases, nameToID, srcID); found {
					edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
					continue
				}
			}

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
