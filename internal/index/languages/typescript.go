package languages

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// tsExtractor serves TypeScript, TSX and JavaScript, which are parsed by TWO
// different grammars — and a tree-sitter query is compiled against one grammar
// and matches nothing on a tree built by another.
//
// DECISION(2026-09): the grammar is chosen per language instead of hardcoded.
// Edges compiled its query against the TypeScript grammar unconditionally while
// the parser gives .js/.jsx/.mjs/.cjs and .tsx a TSX tree, so every call query
// silently found zero matches: NodeBB indexed 3673 symbols and exactly 0 edges,
// which SWE-Explore then scored as the two worst instances of the pilot. Symbols
// survived because that walk compares node kinds as strings, which no grammar
// change can break — so the failure was invisible from the symbol side.
// ASSUMES: the call query's node kinds (call_expression, member_expression,
// property_identifier) are spelled the same in both grammars.
// REVISIT IF: a third grammar joins this extractor — the pairing then wants a
// table rather than a bool.
type tsExtractor struct {
	// tsx selects the TSX grammar, which the parser uses both for .tsx and for
	// JavaScript (TSX is a superset that parses JSX too).
	tsx bool
}

func tsLanguage() *tree_sitter.Language {
	return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
}

func tsxLanguage() *tree_sitter.Language {
	return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX())
}

// queryLanguage returns the grammar this extractor's trees were parsed with. It
// must agree with internal/index/parser.go, which owns that pairing.
func (e *tsExtractor) queryLanguage() *tree_sitter.Language {
	if e.tsx {
		return tsxLanguage()
	}
	return tsLanguage()
}

func NewTSParser() *tree_sitter.Parser {
	p := tree_sitter.NewParser()
	p.SetLanguage(tsLanguage())
	return p
}

// NewTSXParser parses the way .tsx and JavaScript files are parsed in production.
func NewTSXParser() *tree_sitter.Parser {
	p := tree_sitter.NewParser()
	p.SetLanguage(tsxLanguage())
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
			symbols = append(symbols, tsLexicalSymbols(child, source, store.KindConst)...)
		case "variable_declaration":
			symbols = append(symbols, tsLexicalSymbols(child, source, store.KindVar)...)
		case "expression_statement":
			symbols = tsCommonJSSymbols(child, source, symbols)
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
					symbols = append(symbols, tsLexicalSymbols(inner, source, store.KindConst)...)
				case "variable_declaration":
					symbols = append(symbols, tsLexicalSymbols(inner, source, store.KindVar)...)
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

// tsLexicalSymbols reads `const`/`let` (and, with declKind var, `var`)
// declarations.
//
// DECISION(2026-09): a declaration whose VALUE is a function or class is
// indexed as that function or class, body and all, rather than as a bare name.
// `export const handler = async (req) => {...}` is how a large share of modern
// TS/JS states its functions, and it used to reach the index as a name with an
// empty body: measured, every single const symbol had no body — 3280 of 3280 on
// NodeBB, 306 of 306 on NestJS. Two things followed from that. Its code was
// invisible to body FTS and to embeddings, and callableKind() in the retrieval
// pipeline drops const/var as edge targets, so nothing could call it either.
// ASSUMES: a declarator holding a function is meant as a definition, not data.
// REVISIT IF: consts holding functions start dominating for some other reason.
func tsLexicalSymbols(node *tree_sitter.Node, source []byte, declKind string) []store.Symbol {
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
		if tsIsImportBinding(child.ChildByFieldName("value"), source) {
			continue
		}
		doc := tsJSDocComment(node, source)
		sym := store.Symbol{
			Name:          name,
			Kind:          declKind,
			QualifiedName: name,
			StartLine:     int(child.StartPosition().Row) + 1,
			EndLine:       int(child.EndPosition().Row) + 1,
			Signature:     name,
			Docstring:     doc,
		}
		if kind, ok := tsValueKind(child.ChildByFieldName("value")); ok {
			excerpt, fullBody := bodyParts(child, source)
			sym.Kind = kind
			sym.Signature = firstLine(excerpt)
			sym.BodyExcerpt = excerpt
			sym.FullBody = fullBody
			sym.EndLine = int(child.EndPosition().Row) + 1
		}
		result = append(result, sym)
	}
	return result
}

// tsValueKind reports the symbol kind an assigned value deserves, and whether
// it is a definition at all rather than plain data.
func tsValueKind(value *tree_sitter.Node) (string, bool) {
	if value == nil {
		return "", false
	}
	switch value.Kind() {
	case "function_expression", "arrow_function", "function", "generator_function":
		return store.KindFunction, true
	case "class", "class_expression":
		return store.KindClass, true
	}
	return "", false
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
	lang := e.queryLanguage()

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

// tsCommonJSSymbols extracts what the top-level walk cannot see in CommonJS,
// where the code does not live at the top level at all.
//
// DECISION(2026-09): a JavaScript module written as
//
//	module.exports = function (module) {
//	    module.listAppend = async function (key, value) { ... };
//	    async function listPush(key) { ... }
//	};
//
// used to index as ZERO symbols: the walk only inspected top-level children, and
// everything here is one level down inside the wrapper. Measured on NodeBB
// before this: 88.5% of all indexed symbols were `const` require() lines, methods
// were 0.1% (two in the whole repository), and 27.9% of files had no symbol at
// all — the index held the imports and almost none of the code. Java and Python
// indexes of comparable repositories sit at 87% and 60% methods.
//
// Only two shapes are followed, both unambiguous module structure rather than
// ordinary nesting: a function assigned to module.exports/exports (the wrapper),
// and a function assigned to a member expression (the module's own methods).
// Arbitrary nested functions are NOT collected — callbacks and closures would
// bury the real symbols.
// ASSUMES: `module.exports = function(...)` is the dominant CommonJS wrapper.
// REVISIT IF: JS symbol density stays far below the other languages.
func tsCommonJSSymbols(stmt *tree_sitter.Node, source []byte, symbols []store.Symbol) []store.Symbol {
	if body := tsAMDFactoryBody(stmt, source); body != nil {
		return tsWalkTopLevel(body, source, "", symbols)
	}
	assign := tsChildByKind(stmt, "assignment_expression")
	if assign == nil {
		return symbols
	}
	left, right := assign.ChildByFieldName("left"), assign.ChildByFieldName("right")
	if left == nil || right == nil {
		return symbols
	}
	if right.Kind() != "function_expression" && right.Kind() != "arrow_function" &&
		right.Kind() != "function" && right.Kind() != "class" {
		return symbols
	}

	// `module.exports = function (module) { ... }` — the wrapper itself is not a
	// symbol; its BODY is the module's real top level, so walk it as one.
	if target := nodeText(left, source); target == "module.exports" || target == "exports" {
		if body := right.ChildByFieldName("body"); body != nil {
			return tsWalkTopLevel(body, source, "", symbols)
		}
		return symbols
	}

	if left.Kind() != "member_expression" {
		return symbols
	}
	name := nodeText(left.ChildByFieldName("property"), source)
	if name == "" {
		return symbols
	}
	object := nodeText(left.ChildByFieldName("object"), source)
	kind, qname := store.KindMethod, object+"."+name
	// `module.x = ...` inside the wrapper is the module's own export, not a
	// method of some object named "module"; qualifying it that way would put a
	// meaningless prefix on every symbol in the file.
	if object == "module" || object == "exports" || object == "module.exports" {
		kind, qname = store.KindFunction, name
	}

	excerpt, fullBody := bodyParts(assign, source)
	return append(symbols, store.Symbol{
		Name:          name,
		Kind:          kind,
		QualifiedName: qname,
		StartLine:     int(assign.StartPosition().Row) + 1,
		EndLine:       int(assign.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     tsJSDocComment(stmt, source),
		BodyExcerpt:   excerpt,
		FullBody:      fullBody,
	})
}

// tsAMDFactoryBody returns the body of an AMD factory — `define(id, [deps], fn)`
// or `require([deps], fn)` — whose contents are the module's real top level.
//
// DECISION(2026-09): AMD is unwrapped for the same reason CommonJS is, and it is
// not a rare shape: every client-side file in NodeBB is written this way, and
// they were the largest files carrying no symbol at all (public/src/app.js at
// 23KB indexed as nothing). Only `define` and `require` are unwrapped, by name.
// Descending into any call that takes a function would swallow every
// forEach/then/describe callback and bury the real symbols under them.
// ASSUMES: `define`/`require` at statement level mean AMD, not a local helper of
// the same name. REVISIT IF: a codebase defines its own define()/require().
func tsAMDFactoryBody(stmt *tree_sitter.Node, source []byte) *tree_sitter.Node {
	call := tsChildByKind(stmt, "call_expression")
	if call == nil {
		return nil
	}
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Kind() != "identifier" {
		return nil
	}
	switch nodeText(fn, source) {
	case "define", "require":
	default:
		return nil
	}
	args := call.ChildByFieldName("arguments")
	if args == nil {
		return nil
	}
	// The factory is the last function argument; the ones before it are the
	// module id and its dependency list.
	for i := args.ChildCount(); i > 0; i-- {
		arg := args.Child(i - 1)
		if arg == nil {
			continue
		}
		if _, ok := tsValueKind(arg); !ok {
			continue
		}
		if body := arg.ChildByFieldName("body"); body != nil {
			return body
		}
	}
	return nil
}

// tsIsImportBinding reports whether a declaration is just naming an import —
// `const db = require('../database')`.
//
// DECISION(2026-09): require() bindings are not indexed as symbols. Measured on
// NodeBB, they were 51.5% of the whole index and carried no body: `db` appeared
// 285 times, `meta` 165, `user` 146, `privileges` 112 — one identical entry per
// file that imports the module. They cannot be edge targets (callableKind drops
// const/var) and they have no code to match on, but their NAMES are exactly the
// words a question about the system uses, so they competed for the twenty
// result slots against the code that actually implements the thing. An import
// is a reference to a symbol indexed elsewhere, not a definition.
// ASSUMES: the module being imported is itself in the index, where it is
// findable by its real contents. REVISIT IF: cross-file import edges are added
// and need the binding as an anchor.
func tsIsImportBinding(value *tree_sitter.Node, source []byte) bool {
	if value == nil {
		return false
	}
	// `require('x')` and `require('x').Thing` alike.
	for value.Kind() == "member_expression" {
		value = value.ChildByFieldName("object")
		if value == nil {
			return false
		}
	}
	if value.Kind() != "call_expression" {
		return false
	}
	fn := value.ChildByFieldName("function")
	return fn != nil && fn.Kind() == "identifier" && nodeText(fn, source) == "require"
}
