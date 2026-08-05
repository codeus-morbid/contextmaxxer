package languages

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// genericExtractor is the tier-2 language support: a config-driven extractor
// shared by every language whose grammar follows the common tree-sitter shape
// (named declaration nodes with a "name" field).
//
// DECISION(2026-07): call edges are promoted per-language via cfg.calls,
// reusing the conservative resolution proven on Go/Python/TS after the
// hairball incident: exact qualified-name match first, otherwise a UNIQUE
// last-segment suffix — an ambiguous callee name yields no edge. Languages
// without a calls config stay symbols-only. ASSUMES: ~1 edge/symbol on real
// repos (validated 2026-07: gson 0.74, clap 1.65, fmt 0.68, RestSharp 0.65,
// curl 2.06, guzzle 1.16, sinatra 0.67, okio 1.38, os-lib 0.64).
// REVISIT IF: a language's edges/symbol ratio leaves [0.2, 5] on a real repo.
type genericExtractor struct {
	cfg genericConfig
}

type genericConfig struct {
	// containers map declaration node kinds to a symbol kind (class/type/...);
	// the container's name qualifies nested callables (Class.method).
	containers map[string]string
	// callables map node kinds to emit as function (top level) or method
	// (inside a container).
	callables map[string]bool
	// scopeOnly node kinds contribute a qualification scope but emit no
	// symbol themselves (rust impl blocks, c++ namespaces).
	scopeOnly map[string]string // kind -> name field
	// nameOverride resolves the name for node kinds where the default
	// "name" field doesn't apply (C declarator chains).
	nameOverride map[string]func(n *tree_sitter.Node, src []byte) string
	// calls maps call-expression node kinds to a resolver returning the
	// callee's simple name ("" = unresolvable, no edge). Nil = symbols-only.
	calls map[string]func(n *tree_sitter.Node, src []byte) string
}

func (g *genericExtractor) Symbols(tree *tree_sitter.Tree, source []byte) []store.Symbol {
	if tree == nil {
		return nil
	}
	var out []store.Symbol
	g.walk(tree.RootNode(), source, nil, &out)
	return out
}

func (g *genericExtractor) Edges(tree *tree_sitter.Tree, src []byte, nameToID map[string]int64) []store.Edge {
	if tree == nil || g.cfg.calls == nil || len(nameToID) == 0 {
		return nil
	}
	var edges []store.Edge
	g.edgeWalk(tree.RootNode(), src, nil, nameToID, &edges)
	return edges
}

// edgeWalk mirrors the symbol walk's scope tracking; at each named callable it
// attributes every call in the callable's subtree to it (nested closures and
// local functions fold into their enclosing callable, like the py extractor)
// and does not descend further.
func (g *genericExtractor) edgeWalk(n *tree_sitter.Node, src []byte, scope []string, nameToID map[string]int64, edges *[]store.Edge) {
	kind := n.Kind()

	if _, ok := g.cfg.containers[kind]; ok {
		if name := g.nameOf(n, src); name != "" {
			scope = append(scope, name)
		}
	} else if field, ok := g.cfg.scopeOnly[kind]; ok {
		if name := cleanTypeName(nodeText(n.ChildByFieldName(field), src)); name != "" {
			scope = append(scope, name)
		}
	} else if g.cfg.callables[kind] {
		if name := g.nameOf(n, src); name != "" {
			segs := strings.Split(strings.ReplaceAll(name, "::", "."), ".")
			qname := strings.Join(append(append([]string{}, scope...), segs...), ".")
			if srcID, ok := nameToID[qname]; ok {
				g.collectCalls(n, src, srcID, nameToID, edges)
			}
		}
		return
	}

	for i := uint(0); i < n.ChildCount(); i++ {
		g.edgeWalk(n.Child(i), src, scope, nameToID, edges)
	}
}

func (g *genericExtractor) collectCalls(body *tree_sitter.Node, src []byte, srcID int64, nameToID map[string]int64, edges *[]store.Edge) {
	seen := map[string]bool{}
	var visit func(n *tree_sitter.Node)
	visit = func(n *tree_sitter.Node) {
		if resolve, ok := g.cfg.calls[n.Kind()]; ok {
			if callee := resolve(n, src); callee != "" && !seen[callee] {
				seen[callee] = true
				g.emitEdge(callee, srcID, nameToID, edges)
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			visit(n.Child(i))
		}
	}
	visit(body)
}

// emitEdge resolves a callee name with the same conservatism as the Go and
// Python extractors: exact qualified-name hit, else a UNIQUE suffix match —
// >1 same-named symbol means no edge (the Django-hairball lesson).
func (g *genericExtractor) emitEdge(callee string, srcID int64, nameToID map[string]int64, edges *[]store.Edge) {
	if dstID, ok := nameToID[callee]; ok && dstID != srcID {
		*edges = appendEdgeUniq(*edges, store.Edge{Src: srcID, Dst: dstID, Kind: store.EdgeCalls, Weight: 1.0})
		return
	}
	var matchID int64
	count := 0
	for qn, dstID := range nameToID {
		if dstID == srcID {
			continue
		}
		if i := strings.LastIndex(qn, "."); qn[i+1:] == callee {
			matchID = dstID
			count++
			if count > 1 {
				return
			}
		}
	}
	if count == 1 {
		*edges = appendEdgeUniq(*edges, store.Edge{Src: srcID, Dst: matchID, Kind: store.EdgeCalls, Weight: 1.0})
	}
}

// calleeLastSegment normalizes a possibly qualified/generic callee name
// (com.foo.Bar, Foo::bar, App\Job, Vec<T>) down to its simple last segment.
func calleeLastSegment(s string) string {
	s = cleanTypeName(s)
	s = strings.ReplaceAll(s, "::", ".")
	s = strings.ReplaceAll(s, "\\", ".") // php namespace separator
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

func (g *genericExtractor) walk(n *tree_sitter.Node, src []byte, scope []string, out *[]store.Symbol) {
	kind := n.Kind()

	if symKind, ok := g.cfg.containers[kind]; ok {
		if name := g.nameOf(n, src); name != "" {
			*out = append(*out, g.symbol(n, src, scope, name, symKind))
			scope = append(scope, name)
		}
	} else if field, ok := g.cfg.scopeOnly[kind]; ok {
		if name := cleanTypeName(nodeText(n.ChildByFieldName(field), src)); name != "" {
			scope = append(scope, name)
		}
	} else if g.cfg.callables[kind] {
		if name := g.nameOf(n, src); name != "" {
			symKind := store.KindFunction
			if len(scope) > 0 {
				symKind = store.KindMethod
			}
			*out = append(*out, g.symbol(n, src, scope, name, symKind))
		}
		// Callables can nest (closures, local funcs) but naming them through
		// the parent callable adds noise; don't extend the scope.
	}

	for i := uint(0); i < n.ChildCount(); i++ {
		g.walk(n.Child(i), src, scope, out)
	}
}

func (g *genericExtractor) symbol(n *tree_sitter.Node, src []byte, scope []string, name, kind string) store.Symbol {
	// C++ out-of-class definitions carry their own qualifier (Foo::bar).
	segs := strings.Split(strings.ReplaceAll(name, "::", "."), ".")
	simple := segs[len(segs)-1]
	qname := strings.Join(append(append([]string{}, scope...), segs...), ".")

	excerpt := bodyExcerpt(n, src)
	return store.Symbol{
		Name:          simple,
		Kind:          kind,
		QualifiedName: qname,
		StartLine:     int(n.StartPosition().Row) + 1,
		EndLine:       int(n.EndPosition().Row) + 1,
		Signature:     firstLine(excerpt),
		Docstring:     precedingComment(n, src),
		BodyExcerpt:   excerpt,
	}
}

func (g *genericExtractor) nameOf(n *tree_sitter.Node, src []byte) string {
	if fn, ok := g.cfg.nameOverride[n.Kind()]; ok {
		return fn(n, src)
	}
	if name := nodeText(n.ChildByFieldName("name"), src); name != "" {
		return name
	}
	// Some grammars (kotlin) don't attach a "name" field; the declared name
	// is the first identifier-ish direct child.
	return firstIdentChild(n, src)
}

func firstIdentChild(n *tree_sitter.Node, src []byte) string {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		switch c.Kind() {
		case "identifier", "simple_identifier", "type_identifier", "constant":
			return nodeText(c, src)
		}
	}
	return ""
}

// cleanTypeName strips generic parameters from a scope name (impl Foo<T>).
func cleanTypeName(s string) string {
	if i := strings.IndexAny(s, "<("); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// declaratorName digs through C/C++ declarator chains
// (pointer_declarator -> function_declarator -> identifier) to the declared
// name, keeping a qualified_identifier (Foo::bar) intact.
func declaratorName(n *tree_sitter.Node, src []byte) string {
	decl := n.ChildByFieldName("declarator")
	for decl != nil {
		switch decl.Kind() {
		case "identifier", "field_identifier", "type_identifier",
			"qualified_identifier", "operator_name", "destructor_name":
			return nodeText(decl, src)
		}
		if inner := decl.ChildByFieldName("declarator"); inner != nil {
			decl = inner
			continue
		}
		return ""
	}
	return ""
}

// precedingComment collects the contiguous comment block directly above the
// node (the C-family doc convention), stripping comment markers.
//
// DECISION(2026-07): attributes/annotations that sit between the doc and the
// declaration are stepped over, not treated as the end of the block. In
// tree-sitter-rust `#[inline]` is a SIBLING of the function item, so
// `/// doc` + `#[inline]` + `pub fn f()` silently lost its doc — and
// attributes are pervasive in idiomatic Rust (measured on fd: 369 `///`
// lines in source, 68 documented symbols in the index).
func precedingComment(n *tree_sitter.Node, src []byte) string {
	var parts []string
	expectRow := int(n.StartPosition().Row)
	for prev := n.PrevNamedSibling(); prev != nil; prev = prev.PrevNamedSibling() {
		if isAttributeNode(prev.Kind()) {
			// Keep contiguity anchored to the attribute so a blank line
			// between doc and attribute still ends the block.
			if lastContentRow(prev) < expectRow-1 {
				break
			}
			expectRow = int(prev.StartPosition().Row)
			continue
		}
		if !strings.Contains(prev.Kind(), "comment") {
			break
		}
		endRow := lastContentRow(prev)
		if endRow < expectRow-1 {
			break // blank line separates the comment from the declaration
		}
		expectRow = int(prev.StartPosition().Row)
		parts = append([]string{stripCommentMarkers(nodeText(prev, src))}, parts...)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// lastContentRow is the row of a node's last line of actual text.
//
// DECISION(2026-07): a node whose end position sits at column 0 has consumed
// the trailing newline, so its text ends on the PREVIOUS row — tree-sitter's
// rust line_comment and go comment both do this. Comparing raw EndPosition
// rows made every "is there a blank line between doc and declaration" check
// off by one, silently attaching unrelated comments as documentation.
func lastContentRow(n *tree_sitter.Node) int {
	end := n.EndPosition()
	row := int(end.Row)
	if end.Column == 0 && row > 0 {
		row--
	}
	return row
}

// isAttributeNode reports whether a node is an attribute/annotation that may
// legitimately sit between a doc comment and the declaration it documents
// (Rust #[...], C# [...], Java/Kotlin @... when the grammar emits it as a
// sibling rather than a modifier child).
func isAttributeNode(kind string) bool {
	switch kind {
	case "attribute_item", "attribute_list", "annotation", "marker_annotation", "decorator":
		return true
	}
	return false
}

func stripCommentMarkers(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "/*") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "/*"), "*/")
		var lines []string
		for _, ln := range strings.Split(s, "\n") {
			lines = append(lines, strings.TrimPrefix(strings.TrimSpace(ln), "*"))
		}
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		ln = strings.TrimPrefix(ln, "///")
		ln = strings.TrimPrefix(ln, "//")
		ln = strings.TrimPrefix(ln, "#")
		lines = append(lines, strings.TrimSpace(ln))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// Callee resolvers shared by the C-family call configs. Each returns the
// callee's simple name or "" (dynamic/complex callees produce no edge).

// fieldText reads a named child field as text.
func fieldText(n *tree_sitter.Node, field string, src []byte) string {
	return nodeText(n.ChildByFieldName(field), src)
}

// javaMethodCallee: method_invocation name field (obj.foo() -> foo).
func javaMethodCallee(n *tree_sitter.Node, src []byte) string {
	return fieldText(n, "name", src)
}

// newTypeCallee: object_creation_expression type (new com.foo.Bar<T>() -> Bar).
// Resolves to the class symbol; constructors also match via Class.Class suffix
// only when unique, so this conservatively links `new X()` to the type.
func newTypeCallee(n *tree_sitter.Node, src []byte) string {
	return calleeLastSegment(fieldText(n, "type", src))
}

// cFamilyCallee: call_expression function field across C/C++/Rust shapes.
func cFamilyCallee(n *tree_sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	for fn != nil {
		switch fn.Kind() {
		case "identifier":
			return nodeText(fn, src)
		case "scoped_identifier", "qualified_identifier": // rust Foo::bar, c++ ns::f
			return calleeLastSegment(nodeText(fn, src))
		case "field_expression": // x.foo() / x->foo()
			return fieldText(fn, "field", src)
		case "generic_function", "template_function": // foo::<T>() / foo<T>()
			fn = fn.ChildByFieldName("function")
			continue
		}
		return ""
	}
	return ""
}

// csharpCallee: invocation_expression function field.
func csharpCallee(n *tree_sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Kind() {
	case "identifier":
		return nodeText(fn, src)
	case "member_access_expression": // obj.Foo() -> Foo
		return fieldText(fn, "name", src)
	}
	return ""
}

// phpNameCallee: member_call_expression / scoped_call_expression name field
// ($obj->foo(), Foo::bar() -> foo, bar).
func phpNameCallee(n *tree_sitter.Node, src []byte) string {
	return fieldText(n, "name", src)
}

// phpFunctionCallee: function_call_expression function field; plain (name) or
// a namespaced qualified_name (App\load -> load).
func phpFunctionCallee(n *tree_sitter.Node, src []byte) string {
	fn := n.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Kind() {
	case "name", "qualified_name":
		return calleeLastSegment(nodeText(fn, src))
	}
	return "" // variable/closure calls are dynamic
}

// phpNewCallee: object_creation_expression carries no field; the class is the
// first (qualified_)name child (new Widget(), new App\Widget()).
func phpNewCallee(n *tree_sitter.Node, src []byte) string {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Kind() == "name" || c.Kind() == "qualified_name" {
			return calleeLastSegment(nodeText(c, src))
		}
	}
	return ""
}

// rubyCallee: call nodes with an explicit method field (obj.foo, foo()).
// Widget.new resolves to the Widget class symbol instead of the useless
// "new". Bare no-paren calls parse as plain identifiers and are skipped —
// too ambiguous to link.
func rubyCallee(n *tree_sitter.Node, src []byte) string {
	m := n.ChildByFieldName("method")
	if m == nil {
		return ""
	}
	name := nodeText(m, src)
	if name == "new" {
		if r := n.ChildByFieldName("receiver"); r != nil && r.Kind() == "constant" {
			return nodeText(r, src)
		}
	}
	return name
}

// kotlinCallee: call_expression has no fields; the callee is the leading
// identifier (foo()) or the last identifier of a navigation_expression
// (obj.foo()). Widget() constructor calls resolve to the class symbol.
func kotlinCallee(n *tree_sitter.Node, src []byte) string {
	c := n.Child(0)
	if c == nil {
		return ""
	}
	switch c.Kind() {
	case "identifier", "simple_identifier":
		return nodeText(c, src)
	case "navigation_expression":
		var last string
		for i := uint(0); i < c.ChildCount(); i++ {
			gc := c.Child(i)
			if gc.Kind() == "identifier" || gc.Kind() == "simple_identifier" {
				last = nodeText(gc, src)
			}
		}
		return last
	}
	return ""
}

// scalaNewCallee: instance_expression (new Widget()) names the class in its
// first type_identifier child.
func scalaNewCallee(n *tree_sitter.Node, src []byte) string {
	for i := uint(0); i < n.ChildCount(); i++ {
		c := n.Child(i)
		if c.Kind() == "type_identifier" {
			return cleanTypeName(nodeText(c, src))
		}
	}
	return ""
}

// Per-language tier-2 configs. Node kinds follow each official grammar.
var genericConfigs = map[string]genericConfig{
	"java": {
		containers: map[string]string{
			"class_declaration":           store.KindClass,
			"interface_declaration":       store.KindInterface,
			"enum_declaration":            store.KindType,
			"record_declaration":          store.KindClass,
			"annotation_type_declaration": store.KindType,
		},
		callables: map[string]bool{"method_declaration": true, "constructor_declaration": true},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"method_invocation":          javaMethodCallee,
			"object_creation_expression": newTypeCallee,
		},
	},
	"rust": {
		containers: map[string]string{
			"struct_item": store.KindType,
			"enum_item":   store.KindType,
			"trait_item":  store.KindInterface,
			"union_item":  store.KindType,
		},
		// function_signature_item covers bodyless trait-method declarations.
		callables: map[string]bool{"function_item": true, "function_signature_item": true},
		scopeOnly: map[string]string{"impl_item": "type"},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"call_expression": cFamilyCallee,
		},
	},
	"c": {
		containers: map[string]string{
			"struct_specifier": store.KindType,
			"enum_specifier":   store.KindType,
			"union_specifier":  store.KindType,
		},
		callables: map[string]bool{"function_definition": true},
		nameOverride: map[string]func(*tree_sitter.Node, []byte) string{
			"function_definition": declaratorName,
			// struct/enum/union references (no body) must not become symbols.
			"struct_specifier": namedSpecifierWithBody,
			"enum_specifier":   namedSpecifierWithBody,
			"union_specifier":  namedSpecifierWithBody,
		},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"call_expression": cFamilyCallee,
		},
	},
	"cpp": {
		containers: map[string]string{
			"struct_specifier": store.KindType,
			"enum_specifier":   store.KindType,
			"union_specifier":  store.KindType,
			"class_specifier":  store.KindClass,
		},
		callables: map[string]bool{"function_definition": true},
		scopeOnly: map[string]string{"namespace_definition": "name"},
		nameOverride: map[string]func(*tree_sitter.Node, []byte) string{
			"function_definition": declaratorName,
			"struct_specifier":    namedSpecifierWithBody,
			"enum_specifier":      namedSpecifierWithBody,
			"union_specifier":     namedSpecifierWithBody,
			"class_specifier":     namedSpecifierWithBody,
		},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"call_expression": cFamilyCallee,
		},
	},
	"csharp": {
		containers: map[string]string{
			"class_declaration":     store.KindClass,
			"interface_declaration": store.KindInterface,
			"struct_declaration":    store.KindType,
			"enum_declaration":      store.KindType,
			"record_declaration":    store.KindClass,
		},
		callables: map[string]bool{"method_declaration": true, "constructor_declaration": true},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"invocation_expression":      csharpCallee,
			"object_creation_expression": newTypeCallee,
		},
	},
	"ruby": {
		containers: map[string]string{
			"class":  store.KindClass,
			"module": store.KindType,
		},
		callables: map[string]bool{"method": true, "singleton_method": true},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"call": rubyCallee,
		},
	},
	"php": {
		containers: map[string]string{
			"class_declaration":     store.KindClass,
			"interface_declaration": store.KindInterface,
			"trait_declaration":     store.KindClass,
			"enum_declaration":      store.KindType,
		},
		callables: map[string]bool{"function_definition": true, "method_declaration": true},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"function_call_expression":        phpFunctionCallee,
			"member_call_expression":          phpNameCallee,
			"scoped_call_expression":          phpNameCallee,
			"nullsafe_member_call_expression": phpNameCallee,
			"object_creation_expression":      phpNewCallee,
		},
	},
	"kotlin": {
		containers: map[string]string{
			"class_declaration":  store.KindClass,
			"object_declaration": store.KindClass,
		},
		callables: map[string]bool{"function_declaration": true},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"call_expression": kotlinCallee,
		},
	},
	"scala": {
		containers: map[string]string{
			"class_definition":  store.KindClass,
			"object_definition": store.KindClass,
			"trait_definition":  store.KindInterface,
			"enum_definition":   store.KindType,
		},
		// function_declaration covers bodyless (abstract) trait methods.
		callables: map[string]bool{"function_definition": true, "function_declaration": true},
		calls: map[string]func(*tree_sitter.Node, []byte) string{
			"call_expression":     cFamilyCallee,
			"instance_expression": scalaNewCallee,
		},
	},
}

// namedSpecifierWithBody names a struct/class/enum/union specifier only when
// it declares a body — `struct Foo x;` is a reference, not a declaration.
func namedSpecifierWithBody(n *tree_sitter.Node, src []byte) string {
	if n.ChildByFieldName("body") == nil {
		return ""
	}
	return nodeText(n.ChildByFieldName("name"), src)
}
