package languages

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

const tsSource = `
/** Helper function */
function helper(): void {}

class MyService {
  greet(): string {
    helper();
    return "hello";
  }
}

type UserID = string;

const MAX = 42;
`

func TestTSExtractor_Symbols(t *testing.T) {
	p := NewTSParser()
	tree := p.Parse([]byte(tsSource), nil)
	require.NotNil(t, tree)

	ext := &tsExtractor{}
	syms := ext.Symbols(tree, []byte(tsSource))

	byName := make(map[string]string)
	for _, s := range syms {
		byName[s.Name] = s.Kind
	}

	require.Equal(t, "function", byName["helper"])
	require.Equal(t, "class", byName["MyService"])
	require.Equal(t, "method", byName["greet"])
	require.Equal(t, "type", byName["UserID"])
	require.Equal(t, "const", byName["MAX"])
}

func TestTSExtractor_QualifiedNames(t *testing.T) {
	p := NewTSParser()
	tree := p.Parse([]byte(tsSource), nil)
	ext := &tsExtractor{}
	syms := ext.Symbols(tree, []byte(tsSource))

	byQName := make(map[string]bool)
	for _, s := range syms {
		byQName[s.QualifiedName] = true
	}

	require.True(t, byQName["MyService.greet"])
}

func TestTSExtractor_Docstring(t *testing.T) {
	p := NewTSParser()
	tree := p.Parse([]byte(tsSource), nil)
	ext := &tsExtractor{}
	syms := ext.Symbols(tree, []byte(tsSource))

	for _, s := range syms {
		if s.Name == "helper" {
			require.Contains(t, s.Docstring, "Helper function")
			return
		}
	}
	t.Fatal("helper not found")
}

func TestTSExtractor_Edges(t *testing.T) {
	p := NewTSParser()
	tree := p.Parse([]byte(tsSource), nil)
	ext := &tsExtractor{}
	syms := ext.Symbols(tree, []byte(tsSource))

	nameToID := make(map[string]int64)
	for i, s := range syms {
		nameToID[s.QualifiedName] = int64(i + 1)
	}

	edges := ext.Edges(tree, []byte(tsSource), nameToID)

	greetID := nameToID["MyService.greet"]
	helperID := nameToID["helper"]
	require.NotZero(t, greetID)
	require.NotZero(t, helperID)

	found := false
	for _, e := range edges {
		if e.Src == greetID && e.Dst == helperID && e.Kind == "calls" {
			found = true
			break
		}
	}
	require.True(t, found, "expected edge greet -> helper, edges: %+v", edges)
}

func TestTSExtractor_EdgesUniqueSuffixOnly(t *testing.T) {
	source := []byte("function run() {\n  save();\n}\n")
	p := NewTSParser()
	tree := p.Parse(source, nil)
	ext := &tsExtractor{}

	// Ambiguous suffix (>1 same-named symbol) must not fan out into a hairball.
	ambiguous := ext.Edges(tree, source, map[string]int64{"run": 1, "A.save": 2, "B.save": 3})
	requireNoEdge(t, ambiguous, 1, 2)
	requireNoEdge(t, ambiguous, 1, 3)

	// Unique suffix resolves.
	unique := ext.Edges(tree, source, map[string]int64{"run": 1, "A.save": 2})
	requireEdge(t, unique, 1, 2, 2)
}

func TestTSExtractor_EdgesTypeAwareField(t *testing.T) {
	source := []byte(`export class FooService {
  constructor(private readonly repo: BarRepo) {}
  run() {
    this.repo.find();
    this.helper();
  }
  helper() {}
}
class BarRepo {
  find() {}
  save() {}
}
class OtherRepo {
  find() {}
}
`)
	p := NewTSParser()
	tree := p.Parse(source, nil)
	ext := &tsExtractor{}

	nameToID := map[string]int64{
		"FooService":        1,
		"FooService.run":    2,
		"FooService.helper": 3,
		"BarRepo":           4,
		"BarRepo.find":      5,
		"BarRepo.save":      6,
		"OtherRepo":         7,
		"OtherRepo.find":    8,
	}
	edges := ext.Edges(tree, source, nameToID)

	// this.repo.find(): repo's declared type is BarRepo -> BarRepo.find, NOT
	// OtherRepo.find (the suffix-only resolver would have dropped this ambiguous
	// "find" entirely).
	requireEdge(t, edges, 2, 5, 4)
	requireNoEdge(t, edges, 2, 8)
	// this.helper(): resolves to the enclosing class's method.
	requireEdge(t, edges, 2, 3, 5)
}

// In the TS grammar `export function f()` is an export_statement CONTAINING
// the declaration, so looking left from the declaration finds nothing — the
// JSDoc sits before the wrapper. This dropped docs for the entire exported
// API surface (nestjs: 1092 prose JSDoc blocks in source, 165 indexed).
func TestJSDocOnExportedAndDecoratedDeclarations(t *testing.T) {
	syms := symMap(parseWith(t, "typescript", `
/**
 * Returns a decorator that applies all given decorators.
 */
export function applyDecorators(...decorators: Function[]) {
  return decorators;
}

/**
 * Routes incoming requests.
 */
@Injectable()
export class RouterService {
  /**
   * Resolves a route to its handler.
   */
  resolve(path: string): Handler {
    return null;
  }
}

export function plain(x: number) { return x; }
`))

	require.Contains(t, syms["applyDecorators"].Docstring, "applies all given decorators",
		"JSDoc before an export_statement must reach the exported function")
	require.Contains(t, syms["RouterService"].Docstring, "Routes incoming requests",
		"JSDoc before a decorated exported class must reach the class")
	require.Contains(t, syms["RouterService.resolve"].Docstring, "Resolves a route",
		"JSDoc on a plain method must still work")
	require.Empty(t, syms["plain"].Docstring)
}

func TestBlankLineDetachesJSDoc(t *testing.T) {
	syms := symMap(parseWith(t, "typescript", `
/**
 * Documents the next declaration.
 */
export function attached(x: number) { return x; }

/**
 * Floating block, separated by a blank line.
 */

export function detached(x: number) { return x; }
`))
	require.Contains(t, syms["attached"].Docstring, "Documents the next declaration")
	require.Empty(t, syms["detached"].Docstring)
}

// jsSource is JavaScript, which production parses with the TSX grammar. One
// edge is expected: Widget.render -> helper.
const jsSource = `
function helper() {}

class Widget {
  render() {
    helper();
    return 1;
  }
}
`

// Goes through New() rather than constructing the extractor directly: the
// language-to-grammar pairing IS the thing under test, and picking the
// extractor by hand would skip the decision that was wrong.
func TestExtractor_JavaScriptAndTSXProduceEdges(t *testing.T) {
	for _, language := range []string{"javascript", "tsx"} {
		t.Run(language, func(t *testing.T) {
			ext, ok := New(language)
			require.True(t, ok, "no extractor for %s", language)

			source := []byte(jsSource)
			tree := NewTSXParser().Parse(source, nil)
			syms := ext.Symbols(tree, source)

			nameToID := make(map[string]int64)
			for i, s := range syms {
				nameToID[s.QualifiedName] = int64(i + 1)
			}
			renderID := nameToID["Widget.render"]
			helperID := nameToID["helper"]
			require.NotZero(t, renderID, "symbols: %+v", nameToID)
			require.NotZero(t, helperID, "symbols: %+v", nameToID)

			edges := ext.Edges(tree, source, nameToID)
			requireEdge(t, edges, renderID, helperID, 6)
		})
	}
}

// commonJSSource is the shape NodeBB is written in: nothing lives at the top
// level except the wrapper. Worked out by hand before the code: listPrepend
// (assigned to a property) and listPush (declared inside the wrapper) are the
// two real symbols; before this they were zero.
const commonJSSource = `
'use strict';

module.exports = function (module) {
	const helpers = require('./helpers');

	module.listPrepend = async function (key, value) {
		return helpers.first(key, value);
	};

	async function listPush(key, values) {
		return module.client.push(key, values);
	}
};
`

func TestExtractor_CommonJSModuleIsNotEmpty(t *testing.T) {
	ext, ok := New("javascript")
	require.True(t, ok)

	source := []byte(commonJSSource)
	tree := NewTSXParser().Parse(source, nil)
	syms := ext.Symbols(tree, source)

	byName := map[string]string{}
	for _, s := range syms {
		byName[s.QualifiedName] = s.Kind
	}
	require.Contains(t, byName, "listPrepend", "symbols: %+v", byName)
	require.Contains(t, byName, "listPush", "symbols: %+v", byName)
	require.Equal(t, store.KindFunction, byName["listPrepend"],
		"module.x = fn is a module-level function, not a method of an object named module")
}

// Ordinary nesting must NOT be collected, or every callback becomes a symbol
// and the real ones drown.
func TestExtractor_NestedCallbacksAreNotSymbols(t *testing.T) {
	ext, _ := New("javascript")
	source := []byte(`
function outer() {
	items.forEach(function inner(x) { return x; });
	const local = () => 1;
	return local;
}
`)
	tree := NewTSXParser().Parse(source, nil)

	for _, s := range ext.Symbols(tree, source) {
		require.NotEqual(t, "inner", s.Name, "a callback inside a function body is not a symbol")
	}
}

// amdSource is how every client-side file in NodeBB is written. Worked out by
// hand before the code: navigator and count are vars, navigator.init is a
// method assigned to a property, helper is declared inside the factory — four
// symbols where there were none.
const amdSource = `
'use strict';

define('navigator', ['components'], function (components) {
	var navigator = {};
	var count = 0;

	navigator.scrollActive = false;

	navigator.init = function (cb) {
		return cb(count);
	};

	function helper(x) {
		return components.get(x);
	}

	return navigator;
});
`

func TestExtractor_AMDFactoryIsUnwrapped(t *testing.T) {
	ext, _ := New("javascript")
	source := []byte(amdSource)
	tree := NewTSXParser().Parse(source, nil)

	byName := map[string]string{}
	for _, s := range ext.Symbols(tree, source) {
		byName[s.QualifiedName] = s.Kind
	}
	require.Equal(t, store.KindVar, byName["navigator"], "symbols: %+v", byName)
	require.Equal(t, store.KindVar, byName["count"], "symbols: %+v", byName)
	require.Equal(t, store.KindMethod, byName["navigator.init"], "symbols: %+v", byName)
	require.Equal(t, store.KindFunction, byName["helper"], "symbols: %+v", byName)
	require.NotContains(t, byName, "navigator.scrollActive", "assigning a boolean is not a definition")
}

// An ordinary call taking a callback must NOT be unwrapped, or every
// forEach/then/describe body becomes top level.
func TestExtractor_OnlyAMDCallsAreUnwrapped(t *testing.T) {
	ext, _ := New("javascript")
	source := []byte(`
describe('suite', function () {
	function shouldNotBeIndexed() {}
	var alsoNot = 1;
});
`)
	tree := NewTSXParser().Parse(source, nil)
	for _, s := range ext.Symbols(tree, source) {
		require.NotEqual(t, "shouldNotBeIndexed", s.Name, "describe() is not a module factory")
		require.NotEqual(t, "alsoNot", s.Name, "describe() is not a module factory")
	}
}

// A declaration whose value is a function is the function: it needs its body,
// or it is invisible to body search and cannot be an edge target either.
func TestExtractor_FunctionValuedDeclarationsCarryTheirBody(t *testing.T) {
	ext, _ := New("typescript")
	source := []byte(`
export const handler = async (req) => {
	return req.id;
};

const NAME = "x";
`)
	tree := NewTSParser().Parse(source, nil)

	got := map[string]store.Symbol{}
	for _, s := range ext.Symbols(tree, source) {
		got[s.QualifiedName] = s
	}
	require.Equal(t, store.KindFunction, got["handler"].Kind, "an arrow function in a const is a function")
	require.Contains(t, got["handler"].BodyExcerpt, "req.id", "the body has to reach the index")
	require.Equal(t, store.KindConst, got["NAME"].Kind, "a string constant stays a const")
	require.Empty(t, got["NAME"].BodyExcerpt, "plain data needs no body")
}

// An import binding names a symbol defined elsewhere; indexing it adds an entry
// with no body whose name competes with the real definition.
func TestExtractor_RequireBindingsAreNotSymbols(t *testing.T) {
	ext, _ := New("javascript")
	source := []byte(`
const db = require('../database');
const { each } = require('async');
const posts = require('./posts').core;
const MAX = 42;
const helper = () => 1;
`)
	tree := NewTSXParser().Parse(source, nil)

	got := map[string]string{}
	for _, s := range ext.Symbols(tree, source) {
		got[s.QualifiedName] = s.Kind
	}
	require.NotContains(t, got, "db", "symbols: %+v", got)
	require.NotContains(t, got, "posts", "require(...).x is still an import")
	require.Equal(t, store.KindConst, got["MAX"], "a real constant stays")
	require.Equal(t, store.KindFunction, got["helper"], "a function stays")
}

// TypeScript hides code behind namespaces the same way CommonJS hides it behind
// a factory. Worked out by hand before the fix: f, O, apply and g are the four
// symbols in this source, and every one of them used to be missed.
func TestExtractor_NamespacesAndAmbientDeclarationsAreUnwrapped(t *testing.T) {
	ext, _ := New("typescript")
	source := []byte(`
export namespace S {
	export function f(): number { return 1; }
	interface O { size: number; }
}

declare namespace R {
	function apply(t: Function): any;
}

declare module "m" {
	export function g(): void;
}
`)
	tree := NewTSParser().Parse(source, nil)

	got := map[string]string{}
	for _, s := range ext.Symbols(tree, source) {
		got[s.QualifiedName] = s.Kind
	}
	require.Equal(t, store.KindFunction, got["f"], "symbols: %+v", got)
	require.Equal(t, store.KindInterface, got["O"], "symbols: %+v", got)
	require.Equal(t, store.KindFunction, got["apply"], "declare namespace: %+v", got)
	require.Equal(t, store.KindFunction, got["g"], "declare module: %+v", got)
}

// React expresses composition as JSX, not as calls, so the component graph is
// invisible to a call query. Worked out by hand: Toolbar renders Panel and
// Button — two edges — while <div> is an intrinsic element naming nothing.
func TestExtractor_JSXCompositionProducesEdges(t *testing.T) {
	ext, _ := New("tsx")
	source := []byte(`
export function Toolbar({ onSave }) {
	return (
		<Panel title="x">
			<Button onClick={onSave} />
			<div className="spacer" />
		</Panel>
	);
}

export function Panel(props) { return null; }
export function Button(props) { return null; }
`)
	tree := NewTSXParser().Parse(source, nil)

	nameToID := map[string]int64{}
	for i, s := range ext.Symbols(tree, source) {
		nameToID[s.QualifiedName] = int64(i + 1)
	}
	require.NotZero(t, nameToID["Toolbar"], "symbols: %+v", nameToID)

	// The literal opens with a newline, so <Panel> is line 4 and <Button> line 5.
	requireEdge(t, ext.Edges(tree, source, nameToID), nameToID["Toolbar"], nameToID["Panel"], 4)
	requireEdge(t, ext.Edges(tree, source, nameToID), nameToID["Toolbar"], nameToID["Button"], 5)

	// A project CAN define a symbol called "div"; the intrinsic tag must still be
	// refused. Without the capitalisation filter this edge would appear, so this
	// is what separates having the filter from not having it.
	nameToID["div"] = 99
	requireNoEdge(t, ext.Edges(tree, source, nameToID), nameToID["Toolbar"], 99)
}

// The JSX pattern must not reach the TypeScript grammar, which has no such
// nodes: a query that fails to compile returns no edges at all, which would
// silently disable call edges for every .ts file.
func TestExtractor_PlainTypeScriptStillProducesCallEdges(t *testing.T) {
	ext, _ := New("typescript")
	source := []byte("function helper(): void {}\nfunction run(): void { helper(); }\n")
	tree := NewTSParser().Parse(source, nil)

	requireEdge(t, ext.Edges(tree, source, map[string]int64{"helper": 1, "run": 2}), 2, 1, 2)
}
