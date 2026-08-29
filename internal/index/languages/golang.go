package languages

import (
	"bytes"
	pathpkg "path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	importPrefixes := goImportPrefixes(root, source)
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
		shadowedNames := goFunctionBindingNames(child, source)
		paramTypes := goParamTypes(child, source)

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
			callLine := int(fnNode.StartPosition().Row) + 1
			isSelector := recvNode != nil

			// DECISION(2026-08): an identifier selector is a package call only when
			// the operand is an import qualifier not shadowed anywhere in this
			// function. ASSUMES: import path base or explicit alias matches the
			// indexed package name. REVISIT IF: package-name/path mismatches dominate
			// missed cross-package edges.
			if isSelector && recvNode.Kind() == "identifier" {
				qualifier := nodeText(recvNode, source)
				if prefixes, imported := importPrefixes[qualifier]; imported && !shadowedNames[qualifier] {
					resolved := false
					for _, prefix := range prefixes {
						if dstID, found := nameToID[prefix+"."+callee]; found && dstID != srcID {
							edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
							resolved = true
							break
						}
					}
					if resolved {
						continue
					}
				}
			}

			// Type-aware resolution: `recv.Method()` where recv is the method's own
			// receiver resolves to the receiver type's method. Catches the dominant
			// intra-type dispatch pattern (e.g. CockroachDB's ds.sendToReplicas)
			// that the ambiguous global unique-suffix fallback drops.
			if isSelector && recvName != "" && recvType != "" &&
				recvNode.Kind() == "identifier" && nodeText(recvNode, source) == recvName {
				if dstID, found := nameToID[pkg+"."+recvType+"."+callee]; found && dstID != srcID {
					if seenKey := "recv:" + callee; !seen[seenKey] {
						seen[seenKey] = true
						edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
					}
					continue
				}
			}

			// DECISION(2026-08): the same knowledge, applied to the other named
			// thing whose type the source writes down. `repl.AdminTransferLease()`
			// inside leaseQueue.process had no edge — the receiver branch only
			// knows `lq`, and AdminTransferLease is a name Replica and Store both
			// carry, so the unique-suffix fallback dropped it. TypeScript already
			// binds `this.field` by its declared type; this is the Go counterpart
			// for parameters. ASSUMES: a parameter is not reassigned to another
			// type inside the body. REVISIT IF: locals from `:=` need it too,
			// which would mean inferring constructor return types.
			if isSelector && recvNode.Kind() == "identifier" {
				recvText := nodeText(recvNode, source)
				if t, known := paramTypes[recvText]; known {
					owner := pkg + "." + t
					if strings.Contains(t, ".") {
						// Already package-qualified in the source: *cluster.Settings.
						owner = t
					}
					if dstID, found := nameToID[owner+"."+callee]; found && dstID != srcID {
						// Keyed by receiver: two parameters of known types calling
						// the same method name are two different edges.
						if seenKey := "param:" + recvText + "." + callee; !seen[seenKey] {
							seen[seenKey] = true
							edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
						}
						continue
					}
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
			edges = goResolveCallee(edges, callee, srcID, pkg, isSelector, callLine, nameToID)
		}
		cursor.Close()
	}
	return edges
}

func goImportPrefixes(root *tree_sitter.Node, source []byte) map[string][]string {
	imports := make(map[string][]string)
	var visit func(*tree_sitter.Node)
	visit = func(node *tree_sitter.Node) {
		if node.Kind() == "import_spec" {
			pathNode := node.ChildByFieldName("path")
			if pathNode != nil {
				importPath, err := strconv.Unquote(nodeText(pathNode, source))
				if err == nil {
					base := pathpkg.Base(importPath)
					qualifier := base
					prefixes := []string{base}
					if nameNode := node.ChildByFieldName("name"); nameNode != nil {
						qualifier = nodeText(nameNode, source)
						if qualifier == "_" || qualifier == "." {
							return
						}
						prefixes = []string{qualifier}
						if base != qualifier {
							prefixes = append(prefixes, base)
						}
					}
					imports[qualifier] = prefixes
				}
			}
			return
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			if child := node.Child(i); child != nil {
				visit(child)
			}
		}
	}
	visit(root)
	return imports
}

func goFunctionBindingNames(fn *tree_sitter.Node, source []byte) map[string]bool {
	names := make(map[string]bool)
	addIdentifiers := func(node *tree_sitter.Node) {
		var visit func(*tree_sitter.Node)
		visit = func(n *tree_sitter.Node) {
			if n.Kind() == "identifier" {
				names[nodeText(n, source)] = true
				return
			}
			for i := uint(0); i < n.ChildCount(); i++ {
				if child := n.Child(i); child != nil {
					visit(child)
				}
			}
		}
		if node != nil {
			visit(node)
		}
	}

	var visit func(*tree_sitter.Node)
	visit = func(node *tree_sitter.Node) {
		if node != fn && node.Kind() == "func_literal" {
			return
		}
		switch node.Kind() {
		case "parameter_declaration", "variadic_parameter_declaration":
			for i := uint(0); i < node.ChildCount(); i++ {
				if child := node.Child(i); child != nil && child.Kind() == "identifier" {
					names[nodeText(child, source)] = true
				}
			}
		case "short_var_declaration", "range_clause":
			addIdentifiers(node.ChildByFieldName("left"))
		case "var_spec", "const_spec":
			for i := uint(0); i < node.ChildCount(); i++ {
				if child := node.Child(i); child != nil && child.Kind() == "identifier" {
					names[nodeText(child, source)] = true
				}
			}
		case "type_spec":
			if name := node.ChildByFieldName("name"); name != nil {
				names[nodeText(name, source)] = true
			}
		}
		for i := uint(0); i < node.ChildCount(); i++ {
			if child := node.Child(i); child != nil {
				visit(child)
			}
		}
	}
	visit(fn)
	return names
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
func goResolveCallee(edges []store.Edge, callee string, srcID int64, pkg string, isSelector bool, callLine int, nameToID map[string]int64) []store.Edge {
	if dstID, found := nameToID[callee]; found && dstID != srcID {
		return appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
	}
	if !isSelector {
		// A same-package function (incl. a legitimate shadow of a builtin name)
		// is an exact, precise match.
		if dstID, found := nameToID[pkg+"."+callee]; found && dstID != srcID {
			return appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
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
		return appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: matchID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
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
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindFunction,
		QualifiedName: qname,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	}
}

func goMethodSymbol(node *tree_sitter.Node, source []byte, pkg string) store.Symbol {
	qname := goMethodQualifiedName(node, source, pkg)
	parts := strings.SplitN(qname, ".", 3)
	name := parts[len(parts)-1]
	doc := goPrecedingComment(node, source)
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindMethod,
		QualifiedName: qname,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
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

// goParamTypes maps each parameter of fn to the named type it declares. Only
// what the source writes down — nothing is inferred from assignments.
func goParamTypes(fn *tree_sitter.Node, source []byte) map[string]string {
	params := fn.ChildByFieldName("parameters")
	if params == nil {
		return nil
	}
	var out map[string]string
	for i := range params.ChildCount() {
		decl := params.Child(i)
		if decl == nil {
			continue
		}
		if decl.Kind() != "parameter_declaration" && decl.Kind() != "variadic_parameter_declaration" {
			continue
		}
		typeNode := decl.ChildByFieldName("type")
		if typeNode == nil {
			continue
		}
		typeName := goNamedTypeName(nodeText(typeNode, source))
		if typeName == "" {
			continue
		}
		for j := range decl.ChildCount() {
			c := decl.Child(j)
			if c == nil || c.Kind() != "identifier" {
				continue
			}
			if out == nil {
				out = make(map[string]string)
			}
			out[nodeText(c, source)] = typeName
		}
	}
	return out
}

// goNamedTypeName reduces a declared type to the name a method set hangs off: a
// named type, optionally a pointer to one, optionally package-qualified.
// Anything else — slices, maps, channels, funcs, generics, inline structs — has
// no qualified name in the index to bind a call to.
func goNamedTypeName(text string) string {
	t := strings.TrimSpace(text)
	t = strings.TrimSpace(strings.TrimPrefix(t, "*"))
	if t == "" {
		return ""
	}
	for _, r := range t {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' {
			return ""
		}
	}
	return t
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
		excerpt, fullBody := bodyParts(child, source)
		result = append(result, store.Symbol{
			Name:          name,
			Kind:          kind,
			QualifiedName: qname,
			StartLine:     int(child.StartPosition().Row) + 1,
			EndLine:       int(child.EndPosition().Row) + 1,
			Signature:     firstLine(excerpt),
			Docstring:     doc,
			BodyExcerpt:   excerpt,
			FullBody:      fullBody,
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

const bodyExcerptLimit = 2000

func bodyParts(node *tree_sitter.Node, source []byte) (excerpt, fullBody string) {
	start := node.StartByte()
	end := node.EndByte()
	if end > uint(len(source)) {
		end = uint(len(source))
	}
	b := source[start:end]
	if len(b) > bodyExcerptLimit {
		cut := bodyExcerptLimit
		for cut > 0 && !utf8.Valid(b[:cut]) {
			cut--
		}
		return string(b[:cut]) + "\n// ... [truncated]", string(b)
	}
	return string(b), ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := bytes.IndexByte([]byte(s), '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}
