package languages

import (
	"testing"

	"github.com/stretchr/testify/require"
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
