package languages

import (
	"bytes"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type goExtractor struct{}

func goLanguage() *tree_sitter.Language {
	return tree_sitter.NewLanguage(tree_sitter_go.Language())
}

func NewGoParser() *tree_sitter.Parser {
	p := tree_sitter.NewParser()
	p.SetLanguage(goLanguage())
	return p
}

func (e *goExtractor) Symbols(tree *tree_sitter.Tree, source []byte) []store.Symbol {
	root := tree.RootNode()
	pkg := goPackageName(root, source)

	var symbols []store.Symbol
	for i := range root.ChildCount() {
		child := root.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "function_declaration":
			symbols = append(symbols, goFuncSymbol(child, source, pkg))
		case "method_declaration":
			symbols = append(symbols, goMethodSymbol(child, source, pkg))
		case "type_declaration":
			symbols = append(symbols, goTypeSymbols(child, source, pkg)...)
		case "const_declaration":
			symbols = append(symbols, goConstSymbols(child, source, pkg)...)
		}
	}
	return symbols
}

func (e *goExtractor) Edges(tree *tree_sitter.Tree, source []byte, nameToID map[string]int64) []store.Edge {
	if len(nameToID) == 0 {
		return nil
	}

	root := tree.RootNode()
	pkg := goPackageName(root, source)
	lang := goLanguage()

	// Capture the selector operand too, so we can resolve receiver-method calls
	// (recv.Method()) by the receiver's type — see the type-aware branch below.
	q, qErr := tree_sitter.NewQuery(lang,
		`(call_expression function: [(identifier) @fn (selector_expression operand: (_) @recv field: (field_identifier) @fn)])`)
	if qErr != nil {
		return nil
	}
	defer q.Close()

	var edges []store.Edge

	for i := range root.ChildCount() {
		child := root.Child(i)
		if child == nil {
			continue
		}
		if child.Kind() != "function_declaration" && child.Kind() != "method_declaration" {
			continue
		}

		var srcQName, recvName, recvType string
		switch child.Kind() {
		case "function_declaration":
			name := nodeText(child.ChildByFieldName("name"), source)
			srcQName = pkg + "." + name
		case "method_declaration":
			srcQName = goMethodQualifiedName(child, source, pkg)
			if recv := child.ChildByFieldName("receiver"); recv != nil {
				recvName = goReceiverName(recv, source)
				recvType = goReceiverType(recv, source)
			}
		}
		srcID, ok := nameToID[srcQName]
		if !ok {
			continue
		}

		body := child.ChildByFieldName("body")
		if body == nil {
			continue
		}

		cursor := tree_sitter.NewQueryCursor()
		matches := cursor.Matches(q, body, source)
		seen := map[string]bool{}
		for {
			m := matches.Next()
			if m == nil {
				break
			}
			// Per match: the field_identifier capture is the callee (@fn); any
			// other capture is the selector operand (@recv). A plain identifier
			// call has only @fn.
			var fnNode, otherNode *tree_sitter.Node
			for k := range m.Captures {
				n := &m.Captures[k].Node
				if n.Kind() == "field_identifier" {
					fnNode = n
				} else {
					otherNode = n
				}
			}
			recvNode := otherNode
			if fnNode == nil {
				fnNode, recvNode = otherNode, nil // plain identifier call
			}
			if fnNode == nil {
				continue
			}
			callee := nodeText(fnNode, source)
			isSelector := recvNode != nil

			// Type-aware resolution: `recv.Method()` where recv is the method's own
			// receiver resolves to the receiver type's method. Catches the dominant
			// intra-type dispatch pattern (e.g. CockroachDB's ds.sendToReplicas)
			// that the ambiguous global unique-suffix fallback drops.
			if isSelector && recvName != "" && recvType != "" &&
				recvNode.Kind() == "identifier" && nodeText(recvNode, source) == recvName {
				if dstID, found := nameToID[pkg+"."+recvType+"."+callee]; found && dstID != srcID {
					if seenKey := "recv:" + callee; !seen[seenKey] {
						seen[seenKey] = true
						edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0})
					}
					continue
				}
			}

			seenKey := callee
			if isSelector {
				seenKey = "." + callee
			}
			if seen[seenKey] {
				continue
			}
			seen[seenKey] = true
			edges = goResolveCallee(edges, callee, srcID, pkg, isSelector, nameToID)
		}
		cursor.Close()
	}
	return edges
}

// goReceiverName returns the receiver variable name of a method (the `ds` in
// `(ds *DistSender)`), or "" if unnamed.
func goReceiverName(paramList *tree_sitter.Node, source []byte) string {
	for i := range paramList.ChildCount() {
		child := paramList.Child(i)
		if child == nil || child.Kind() != "parameter_declaration" {
			continue
		}
		if nameNode := child.ChildByFieldName("name"); nameNode != nil {
			return nodeText(nameNode, source)
		}
		for j := range child.ChildCount() {
			c := child.Child(j)
			if c != nil && c.Kind() == "identifier" {
				return nodeText(c, source)
			}
		}
	}
	return ""
}

// goBuiltins are Go's predeclared functions. A call to one is never an edge to a
// graph symbol, so the suffix fallback must not bind a bare `len(x)` / `make(...)`
// to an unrelated same-named function or method.
var goBuiltins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// goResolveCallee resolves only when there is a single clear target.
func goResolveCallee(edges []store.Edge, callee string, srcID int64, pkg string, isSelector bool, nameToID map[string]int64) []store.Edge {
	if dstID, found := nameToID[callee]; found && dstID != srcID {
		return appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0})
	}
	if !isSelector {
		// A same-package function (incl. a legitimate shadow of a builtin name)
		// is an exact, precise match.
		if dstID, found := nameToID[pkg+"."+callee]; found && dstID != srcID {
			return appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0})
		}
		// Past the exact match, a bare `callee(...)` is a Go builtin — never a
		// graph symbol. Don't let the suffix fallback bind it to a stray
		// same-named func/method in another package (e.g. len -> memChunk.len).
		if goBuiltins[callee] {
			return edges
		}
	}
	var matchID int64
	matches := 0
	for qn, dstID := range nameToID {
		if dstID == srcID {
			continue
		}
		parts := strings.Split(qn, ".")
		if parts[len(parts)-1] != callee {
			continue
		}
		// A non-selector call `foo()` can only target a package-level function
		// (`pkg.foo`, 2 parts) — never a method (`pkg.Type.foo`, 3 parts), which
		// requires a receiver/selector.
		if !isSelector && len(parts) != 2 {
			continue
		}
		matchID = dstID
		matches++
	}
	if matches == 1 {
		return appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: matchID, Kind: store.EdgeCalls, Weight: 1.0})
	}
	return edges
}

func appendEdgeUniq(edges []store.Edge, e store.Edge) []store.Edge {
	for _, ex := range edges {
		if ex.Src == e.Src && ex.Dst == e.Dst && ex.Kind == e.Kind {
			return edges
		}
	}
	return append(edges, e)
}

func goPackageName(root *tree_sitter.Node, source []byte) string {
	for i := range root.ChildCount() {
		child := root.Child(i)
		if child != nil && child.Kind() == "package_clause" {
			for j := range child.ChildCount() {
				c := child.Child(j)
				if c != nil && c.Kind() == "package_identifier" {
					return nodeText(c, source)
				}
			}
		}
	}
	return ""
}

func goFuncSymbol(node *tree_sitter.Node, source []byte, pkg string) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	qname := pkg + "." + name
	doc := goPrecedingComment(node, source)
	excerpt := bodyExcerpt(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindFunction,
		QualifiedName: qname,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
	}
}

func goMethodSymbol(node *tree_sitter.Node, source []byte, pkg string) store.Symbol {
	qname := goMethodQualifiedName(node, source, pkg)
	parts := strings.SplitN(qname, ".", 3)
	name := parts[len(parts)-1]
	doc := goPrecedingComment(node, source)
	excerpt := bodyExcerpt(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindMethod,
		QualifiedName: qname,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
	}
}

func goMethodQualifiedName(node *tree_sitter.Node, source []byte, pkg string) string {
	receiver := node.ChildByFieldName("receiver")
	receiverType := ""
	if receiver != nil {
		receiverType = goReceiverType(receiver, source)
	}
	name := nodeText(node.ChildByFieldName("name"), source)
	if receiverType != "" {
		return pkg + "." + receiverType + "." + name
	}
	return pkg + "." + name
}

func goReceiverType(paramList *tree_sitter.Node, source []byte) string {
	for i := range paramList.ChildCount() {
		child := paramList.Child(i)
		if child == nil || child.Kind() != "parameter_declaration" {
			continue
		}
		typeNode := child.ChildByFieldName("type")
		if typeNode != nil {
			t := nodeText(typeNode, source)
			return strings.TrimPrefix(t, "*")
		}
		for j := range child.ChildCount() {
			c := child.Child(j)
			if c != nil && (c.Kind() == "type_identifier" || c.Kind() == "pointer_type") {
				return strings.TrimPrefix(nodeText(c, source), "*")
			}
		}
	}
	return ""
}

func goTypeSymbols(node *tree_sitter.Node, source []byte, pkg string) []store.Symbol {
	var result []store.Symbol
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil || child.Kind() != "type_spec" {
			continue
		}
		name := nodeText(child.ChildByFieldName("name"), source)
		typeNode := child.ChildByFieldName("type")
		kind := store.KindType
		if typeNode != nil {
			switch typeNode.Kind() {
			case "struct_type":
				kind = store.KindClass
			case "interface_type":
				kind = store.KindInterface
			}
		}
		qname := pkg + "." + name
		doc := goPrecedingComment(node, source)
		excerpt := bodyExcerpt(child, source)
		result = append(result, store.Symbol{
			Name:          name,
			Kind:          kind,
			QualifiedName: qname,
			StartLine:     int(child.StartPosition().Row) + 1,
			EndLine:       int(child.EndPosition().Row) + 1,
			Signature:     firstLine(excerpt),
			Docstring:     doc,
			BodyExcerpt:   excerpt,
		})
	}
	return result
}

func goConstSymbols(node *tree_sitter.Node, source []byte, pkg string) []store.Symbol {
	var result []store.Symbol
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil || child.Kind() != "const_spec" {
			continue
		}
		for j := range child.ChildCount() {
			nameNode := child.Child(j)
			if nameNode != nil && nameNode.Kind() == "identifier" {
				name := nodeText(nameNode, source)
				doc := goPrecedingComment(node, source)
				result = append(result, store.Symbol{
					Name:          name,
					Kind:          store.KindConst,
					QualifiedName: pkg + "." + name,
					StartLine:     int(child.StartPosition().Row) + 1,
					EndLine:       int(child.EndPosition().Row) + 1,
					Signature:     name,
					Docstring:     doc,
				})
			}
		}
	}
	return result
}

// goPrecedingComment collects the doc block directly above a declaration.
//
// DECISION(2026-07): contiguity is enforced. Without it any comment sibling
// became the doc regardless of distance, so a license header or an unrelated
// note separated by blank lines was indexed as the symbol's documentation —
// noise in the embedding text, the reranker document and the agent payload.
func goPrecedingComment(node *tree_sitter.Node, source []byte) string {
	var lines []string
	expectRow := int(node.StartPosition().Row)
	for prev := node.PrevNamedSibling(); prev != nil && prev.Kind() == "comment"; prev = prev.PrevNamedSibling() {
		if lastContentRow(prev) < expectRow-1 {
			break // blank line separates it from what follows
		}
		expectRow = int(prev.StartPosition().Row)
		lines = append([]string{nodeText(prev, source)}, lines...)
	}
	return strings.Join(lines, "\n")
}

func nodeText(node *tree_sitter.Node, source []byte) string {
	if node == nil {
		return ""
	}
	start := node.StartByte()
	end := node.EndByte()
	if end > uint(len(source)) {
		end = uint(len(source))
	}
	return string(source[start:end])
}

func bodyExcerpt(node *tree_sitter.Node, source []byte) string {
	start := node.StartByte()
	end := node.EndByte()
	if end > uint(len(source)) {
		end = uint(len(source))
	}
	b := source[start:end]
	if len(b) > 2000 {
		return string(b[:2000]) + "\n// ... [truncated]"
	}
	return string(b)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := bytes.IndexByte([]byte(s), '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}
