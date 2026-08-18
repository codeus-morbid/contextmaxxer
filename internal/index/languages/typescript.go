package languages

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type tsExtractor struct{}

func tsLanguage() *tree_sitter.Language {
	return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
}

func NewTSParser() *tree_sitter.Parser {
	p := tree_sitter.NewParser()
	p.SetLanguage(tsLanguage())
	return p
}

func (e *tsExtractor) Symbols(tree *tree_sitter.Tree, source []byte) []store.Symbol {
	root := tree.RootNode()
	var symbols []store.Symbol
	symbols = tsWalkTopLevel(root, source, "", symbols)
	return symbols
}

func tsWalkTopLevel(node *tree_sitter.Node, source []byte, className string, symbols []store.Symbol) []store.Symbol {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil {
			continue
		}
		switch child.Kind() {
		case "function_declaration":
			symbols = append(symbols, tsFuncSymbol(child, source))
		case "class_declaration":
			sym := tsClassSymbol(child, source)
			symbols = append(symbols, sym)
			body := child.ChildByFieldName("body")
			if body != nil {
				symbols = tsWalkClassBody(body, source, sym.Name, symbols)
			}
		case "interface_declaration":
			symbols = append(symbols, tsInterfaceSymbol(child, source))
		case "type_alias_declaration":
			symbols = append(symbols, tsTypeAliasSymbol(child, source))
		case "lexical_declaration":
			symbols = append(symbols, tsLexicalSymbols(child, source)...)
		case "export_statement":
			// unwrap export default/named declarations
			for j := range child.ChildCount() {
				inner := child.Child(j)
				if inner == nil {
					continue
				}
				switch inner.Kind() {
				case "function_declaration":
					symbols = append(symbols, tsFuncSymbol(inner, source))
				case "class_declaration":
					sym := tsClassSymbol(inner, source)
					symbols = append(symbols, sym)
					body := inner.ChildByFieldName("body")
					if body != nil {
						symbols = tsWalkClassBody(body, source, sym.Name, symbols)
					}
				case "interface_declaration":
					symbols = append(symbols, tsInterfaceSymbol(inner, source))
				case "type_alias_declaration":
					symbols = append(symbols, tsTypeAliasSymbol(inner, source))
				case "lexical_declaration":
					symbols = append(symbols, tsLexicalSymbols(inner, source)...)
				}
			}
		}
		_ = className
	}
	return symbols
}

func tsWalkClassBody(body *tree_sitter.Node, source []byte, className string, symbols []store.Symbol) []store.Symbol {
	for i := range body.ChildCount() {
		child := body.Child(i)
		if child == nil {
			continue
		}
		if child.Kind() == "method_definition" {
			symbols = append(symbols, tsMethodSymbol(child, source, className))
		}
	}
	return symbols
}

func tsFuncSymbol(node *tree_sitter.Node, source []byte) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	doc := tsJSDocComment(node, source)
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindFunction,
		QualifiedName: name,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	}
}

func tsClassSymbol(node *tree_sitter.Node, source []byte) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	doc := tsJSDocComment(node, source)
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

func tsMethodSymbol(node *tree_sitter.Node, source []byte, className string) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	qname := className + "." + name
	if className == "" {
		qname = name
	}
	doc := tsJSDocComment(node, source)
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

func tsInterfaceSymbol(node *tree_sitter.Node, source []byte) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	doc := tsJSDocComment(node, source)
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindInterface,
		QualifiedName: name,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	}
}

func tsTypeAliasSymbol(node *tree_sitter.Node, source []byte) store.Symbol {
	name := nodeText(node.ChildByFieldName("name"), source)
	doc := tsJSDocComment(node, source)
	excerpt, fullBody := bodyParts(node, source)
	return store.Symbol{
		Name:          name,
		Kind:          store.KindType,
		QualifiedName: name,
		StartLine:     int(node.StartPosition().Row) + 1,
		EndLine:       int(node.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     doc,
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	}
}

func tsLexicalSymbols(node *tree_sitter.Node, source []byte) []store.Symbol {
	var result []store.Symbol
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil || child.Kind() != "variable_declarator" {
			continue
		}
		nameNode := child.ChildByFieldName("name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, source)
		if name == "" {
			continue
		}
		doc := tsJSDocComment(node, source)
		result = append(result, store.Symbol{
			Name:          name,
			Kind:          store.KindConst,
			QualifiedName: name,
			StartLine:     int(child.StartPosition().Row) + 1,
			EndLine:       int(child.EndPosition().Row) + 1,
			Signature:     name,
			Docstring:     doc,
		})
	}
	return result
}

// tsJSDocComment finds the /** */ block that documents a declaration.
//
// DECISION(2026-07): climb out of wrapper nodes before looking left. In the
// TS grammar `export function f()` is an export_statement CONTAINING the
// function_declaration, and a decorated class is a decorated_definition — so
// the declaration node itself has no previous sibling and the JSDoc, which
// sits before the wrapper, was silently dropped. That lost docs for exactly
// the exported/decorated API surface people search for (measured on nestjs:
// 1092 prose JSDoc blocks in source, 165 docstrings in the index).
// Decorators are also skipped over, since `/** doc */ @Injectable() class X`
// puts the decorator between the comment and the declaration.
func tsJSDocComment(node *tree_sitter.Node, source []byte) string {
	cur := node
	for {
		parent := cur.Parent()
		if parent == nil {
			break
		}
		switch parent.Kind() {
		case "export_statement", "decorated_definition", "ambient_declaration":
			cur = parent
			continue
		}
		break
	}
	expectRow := int(cur.StartPosition().Row)
	for prev := cur.PrevNamedSibling(); prev != nil; prev = prev.PrevNamedSibling() {
		if lastContentRow(prev) < expectRow-1 {
			return "" // blank line: the comment documents something else
		}
		switch prev.Kind() {
		case "comment":
			text := nodeText(prev, source)
			if strings.HasPrefix(text, "/**") {
				return text
			}
			return ""
		case "decorator":
			expectRow = int(prev.StartPosition().Row)
			continue // `/** doc */ @Dec() export class X`
		default:
			return ""
		}
	}
	return ""
}

func (e *tsExtractor) Edges(tree *tree_sitter.Tree, source []byte, nameToID map[string]int64) []store.Edge {
	if len(nameToID) == 0 {
		return nil
	}

	root := tree.RootNode()
	lang := tsLanguage()

	// Capture the member-expression receiver too, so `this.field.method()` /
	// `this.method()` can resolve by the field's DECLARED TS type (the DI pattern)
	// instead of a fuzzy global name match — TS writes the type in the source, so
	// no inference is needed.
	q, qErr := tree_sitter.NewQuery(lang,
		`(call_expression function: [(identifier) @fn (member_expression object: (_) @recv property: (property_identifier) @fn)])`)
	if qErr != nil {
		return nil
	}
	defer q.Close()

	var edges []store.Edge
	edges = tsExtractEdgesFromBlock(root, source, q, "", nil, nameToID, edges)
	return edges
}

func tsExtractEdgesFromBlock(node *tree_sitter.Node, source []byte, q *tree_sitter.Query, _ string, _ map[string]string, nameToID map[string]int64, edges []store.Edge) []store.Edge {
	for i := range node.ChildCount() {
		child := node.Child(i)
		if child == nil {
			continue
		}
		// `export class Foo {}` / `export function f() {}` wrap the declaration in
		// an export_statement — handle the inner decl directly (recursing into the
		// class_declaration would iterate ITS children and miss it).
		if child.Kind() == "export_statement" {
			for j := range child.ChildCount() {
				edges = tsHandleDecl(child.Child(j), source, q, nameToID, edges)
			}
			continue
		}
		edges = tsHandleDecl(child, source, q, nameToID, edges)
	}
	return edges
}

func tsHandleDecl(child *tree_sitter.Node, source []byte, q *tree_sitter.Query, nameToID map[string]int64, edges []store.Edge) []store.Edge {
	if child == nil {
		return edges
	}
	switch child.Kind() {
	case "function_declaration":
		return tsExtractCallsInFunc(child, source, q, "", nil, nameToID, edges)
	case "class_declaration":
		cname := nodeText(child.ChildByFieldName("name"), source)
		body := child.ChildByFieldName("body")
		if body != nil {
			ft := tsClassFieldTypes(body, source)
			for j := range body.ChildCount() {
				m := body.Child(j)
				if m != nil && m.Kind() == "method_definition" {
					edges = tsExtractCallsInFunc(m, source, q, cname, ft, nameToID, edges)
				}
			}
		}
	}
	return edges
}

// tsClassFieldTypes maps a class's field names to their declared TS type names —
// from `private foo: Type` fields and constructor parameter-properties
// (`constructor(private readonly foo: Type)`). This is what lets `this.foo.m()`
// resolve to `Type.m`.
func tsClassFieldTypes(body *tree_sitter.Node, source []byte) map[string]string {
	m := map[string]string{}
	for i := range body.ChildCount() {
		c := body.Child(i)
		if c == nil {
			continue
		}
		switch c.Kind() {
		case "public_field_definition":
			name := tsChildText(c, "property_identifier", source)
			t := tsTypeName(tsChildByKind(c, "type_annotation"), source)
			if name != "" && t != "" {
				m[name] = t
			}
		case "method_definition":
			if tsChildText(c, "property_identifier", source) != "constructor" {
				continue
			}
			params := tsChildByKind(c, "formal_parameters")
			if params == nil {
				continue
			}
			for j := range params.ChildCount() {
				p := params.Child(j)
				if p == nil || (p.Kind() != "required_parameter" && p.Kind() != "optional_parameter") {
					continue
				}
				// Only parameter-properties (with an accessibility/readonly
				// modifier) become `this.x` fields.
				if tsChildByKind(p, "accessibility_modifier") == nil && tsChildByKind(p, "readonly") == nil {
					continue
				}
				name := tsChildText(p, "identifier", source)
				t := tsTypeName(tsChildByKind(p, "type_annotation"), source)
				if name != "" && t != "" {
					m[name] = t
				}
			}
		}
	}
	return m
}

func tsExtractCallsInFunc(node *tree_sitter.Node, source []byte, q *tree_sitter.Query, className string, fieldTypes map[string]string, nameToID map[string]int64, edges []store.Edge) []store.Edge {
	srcQName := nodeText(node.ChildByFieldName("name"), source)
	srcID, ok := nameToID[srcQName]
	if !ok && node.Kind() == "method_definition" && className != "" {
		if id, found := nameToID[className+"."+srcQName]; found {
			srcID, ok = id, true
		}
	}
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
		mt := matches.Next()
		if mt == nil {
			break
		}

		// Each match is either a plain `foo()` (one @fn capture) or a member call
		// `recv.foo()` (@recv object + @fn property_identifier).
		var fnNode, recvNode *tree_sitter.Node
		if len(mt.Captures) == 1 {
			fnNode = &mt.Captures[0].Node
		} else {
			for k := range mt.Captures {
				n := &mt.Captures[k].Node
				if n.Kind() == "property_identifier" {
					fnNode = n
				} else {
					recvNode = n
				}
			}
		}
		if fnNode == nil {
			continue
		}
		callee := nodeText(fnNode, source)
		callLine := int(fnNode.StartPosition().Row) + 1

		key := callee
		if recvNode != nil {
			key = nodeText(recvNode, source) + "|" + callee
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		// Type-aware: resolve the receiver to a declared TS type, then bind to
		// `Type.callee`. `this` -> the enclosing class; `this.field` -> the field's
		// type. This is what makes DI-heavy code (NestJS) produce real edges.
		if recvNode != nil {
			var recvType string
			switch recvNode.Kind() {
			case "this":
				recvType = className
			case "member_expression":
				if obj := recvNode.ChildByFieldName("object"); obj != nil && obj.Kind() == "this" {
					recvType = fieldTypes[nodeText(recvNode.ChildByFieldName("property"), source)]
				}
			}
			if recvType != "" {
				if dstID, found := nameToID[recvType+"."+callee]; found && dstID != srcID {
					edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
					continue
				}
			}
		}

		// Fallback: exact name, then UNIQUE suffix only (ambiguous -> no edge, to
		// avoid the hairball).
		if dstID, found := nameToID[callee]; found && dstID != srcID {
			edges = appendEdgeUniq(edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0, CallLine: callLine})
			continue
		}
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
	return edges
}

func tsChildByKind(node *tree_sitter.Node, kind string) *tree_sitter.Node {
	if node == nil {
		return nil
	}
	for i := range node.ChildCount() {
		c := node.Child(i)
		if c != nil && c.Kind() == kind {
			return c
		}
	}
	return nil
}

func tsChildText(node *tree_sitter.Node, kind string, source []byte) string {
	return nodeText(tsChildByKind(node, kind), source)
}

// tsTypeName returns the first type_identifier inside a type_annotation
// (`: Repository<User>` -> "Repository", `: Foo` -> "Foo"); predefined/union
// types yield "".
func tsTypeName(annotation *tree_sitter.Node, source []byte) string {
	return tsFirstDescOfKind(annotation, "type_identifier", source)
}

func tsFirstDescOfKind(node *tree_sitter.Node, kind string, source []byte) string {
	if node == nil {
		return ""
	}
	if node.Kind() == kind {
		return nodeText(node, source)
	}
	for i := range node.ChildCount() {
		if r := tsFirstDescOfKind(node.Child(i), kind, source); r != "" {
			return r
		}
	}
	return ""
}
