package languages

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/require"
)

const goSource = `package mypkg

// MyFunc does something
func MyFunc() {
	B()
}

func B() {}

type MyStruct struct {
	Field string
}

func (s *MyStruct) Method() {}

const MaxRetries = 3
`

func TestGoExtractor_PreservesLosslessBodyOutsideExcerpt(t *testing.T) {
	function := "func Huge() {\n\t_ = \"" + strings.Repeat("ё", 1100) + "\"\n}"
	source := []byte("package mypkg\n\n" + function)
	p := NewGoParser()
	tree := p.Parse(source, nil)
	syms := (&goExtractor{}).Symbols(tree, source)
	require.Len(t, syms, 1)
	require.Contains(t, syms[0].BodyExcerpt, "... [truncated]")
	require.True(t, utf8.ValidString(syms[0].BodyExcerpt))
	require.Equal(t, function, syms[0].FullBody)
}

func TestGoExtractor_Symbols(t *testing.T) {
	p := NewGoParser()
	tree := p.Parse([]byte(goSource), nil)
	require.NotNil(t, tree)

	ext := &goExtractor{}
	syms := ext.Symbols(tree, []byte(goSource))

	require.GreaterOrEqual(t, len(syms), 4)

	byName := make(map[string]string)
	for _, s := range syms {
		byName[s.Name] = s.Kind
	}

	require.Equal(t, "function", byName["MyFunc"])
	require.Equal(t, "function", byName["B"])
	require.Equal(t, "class", byName["MyStruct"])
	require.Equal(t, "method", byName["Method"])
	require.Equal(t, "const", byName["MaxRetries"])
}

func TestGoExtractor_QualifiedNames(t *testing.T) {
	p := NewGoParser()
	tree := p.Parse([]byte(goSource), nil)
	ext := &goExtractor{}
	syms := ext.Symbols(tree, []byte(goSource))

	byQName := make(map[string]bool)
	for _, s := range syms {
		byQName[s.QualifiedName] = true
	}

	require.True(t, byQName["mypkg.MyFunc"])
	require.True(t, byQName["mypkg.B"])
	require.True(t, byQName["mypkg.MyStruct.Method"])
	require.True(t, byQName["mypkg.MyStruct"])
	require.True(t, byQName["mypkg.MaxRetries"])
}

func TestGoExtractor_Docstring(t *testing.T) {
	p := NewGoParser()
	tree := p.Parse([]byte(goSource), nil)
	ext := &goExtractor{}
	syms := ext.Symbols(tree, []byte(goSource))

	for _, s := range syms {
		if s.Name == "MyFunc" {
			require.Contains(t, s.Docstring, "MyFunc does something")
			return
		}
	}
	t.Fatal("MyFunc not found")
}

func TestGoExtractor_Edges(t *testing.T) {
	p := NewGoParser()
	tree := p.Parse([]byte(goSource), nil)
	ext := &goExtractor{}
	syms := ext.Symbols(tree, []byte(goSource))

	nameToID := make(map[string]int64)
	for i, s := range syms {
		nameToID[s.QualifiedName] = int64(i + 1)
	}

	edges := ext.Edges(tree, []byte(goSource), nameToID)

	myFuncID := nameToID["mypkg.MyFunc"]
	bID := nameToID["mypkg.B"]
	require.NotZero(t, myFuncID)
	require.NotZero(t, bID)

	found := false
	for _, e := range edges {
		if e.Src == myFuncID && e.Dst == bID && e.Kind == "calls" {
			found = true
			break
		}
	}
	require.True(t, found, "expected edge MyFunc -> B, edges: %+v", edges)
}

func TestGoExtractor_EdgesResolveSamePackageCrossFileNameMap(t *testing.T) {
	source := []byte(`package mypkg

func Caller() {
	Callee()
}
`)
	p := NewGoParser()
	tree := p.Parse(source, nil)
	ext := &goExtractor{}

	nameToID := map[string]int64{
		"mypkg.Caller": 1,
		"mypkg.Callee": 2,
	}

	edges := ext.Edges(tree, source, nameToID)

	requireEdge(t, edges, 1, 2, 4)
}

func TestGoExtractor_EdgesResolveUniqueSelectorSuffix(t *testing.T) {
	source := []byte(`package mypkg

func Caller(s *Store) {
	s.Save()
}
`)
	p := NewGoParser()
	tree := p.Parse(source, nil)
	ext := &goExtractor{}

	nameToID := map[string]int64{
		"mypkg.Caller":     1,
		"mypkg.Store.Save": 2,
	}

	edges := ext.Edges(tree, source, nameToID)

	requireEdge(t, edges, 1, 2, 4)
}

func TestGoExtractor_EdgesSkipAmbiguousSelectorSuffix(t *testing.T) {
	source := []byte(`package mypkg

func Caller(s *Store) {
	s.Save()
}
`)
	p := NewGoParser()
	tree := p.Parse(source, nil)
	ext := &goExtractor{}

	nameToID := map[string]int64{
		"mypkg.Caller":        1,
		"mypkg.Store.Save":    2,
		"otherpkg.Cache.Save": 3,
	}

	edges := ext.Edges(tree, source, nameToID)

	requireNoEdge(t, edges, 1, 2)
	requireNoEdge(t, edges, 1, 3)
}

func TestGoExtractor_EdgesResolveImportedPackageSelectorDespiteAmbiguousSuffix(t *testing.T) {
	tests := []struct {
		name       string
		importDecl string
		call       string
	}{
		{name: "default import", importDecl: `"example/scrape"`, call: "scrape.NewManager()"},
		{name: "aliased import", importDecl: `prom "example/scrape"`, call: "prom.NewManager()"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte("package main\n\nimport " + tt.importDecl + "\n\nfunc run() {\n\t" + tt.call + "\n}\n")
			p := NewGoParser()
			tree := p.Parse(source, nil)
			ext := &goExtractor{}

			nameToID := map[string]int64{
				"main.run":          1,
				"scrape.NewManager": 2,
				"other.NewManager":  3,
			}

			edges := ext.Edges(tree, source, nameToID)

			requireEdge(t, edges, 1, 2, 6)
			requireNoEdge(t, edges, 1, 3)
		})
	}
}

func TestGoExtractor_EdgesDoNotResolveShadowedImportQualifier(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "parameter", body: "func run(scrape *Manager) {\n\tscrape.NewManager()\n}"},
		{name: "local", body: "func run() {\n\tscrape := makeManager()\n\tscrape.NewManager()\n}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := []byte("package main\n\nimport \"example/scrape\"\n\n" + tt.body + "\n")
			p := NewGoParser()
			tree := p.Parse(source, nil)
			ext := &goExtractor{}

			nameToID := map[string]int64{
				"main.run":          1,
				"scrape.NewManager": 2,
				"other.NewManager":  3,
			}

			edges := ext.Edges(tree, source, nameToID)

			requireNoEdge(t, edges, 1, 2)
			requireNoEdge(t, edges, 1, 3)
		})
	}
}

func TestGoExtractor_EdgesSkipBuiltinCalls(t *testing.T) {
	// len/make/cap are builtins; even when a same-named method is the unique
	// suffix in the symbol table, a builtin call must not create an edge.
	source := []byte(`package mypkg

func URL(v []string) int {
	_ = make([]string, len(v))
	return cap(v)
}
`)
	p := NewGoParser()
	tree := p.Parse(source, nil)
	ext := &goExtractor{}

	nameToID := map[string]int64{
		"mypkg.URL":             1,
		"otherpkg.memChunk.len": 2,
		"otherpkg.buf.cap":      3,
		"otherpkg.pool.make":    4,
	}

	edges := ext.Edges(tree, source, nameToID)

	require.Empty(t, edges, "builtin calls (len/make/cap) must not create edges, got: %+v", edges)
}

func TestGoExtractor_EdgesNonSelectorCallSkipsMethod(t *testing.T) {
	// A bare `process()` call cannot target a method (which needs a receiver),
	// so the suffix fallback must not bind it to otherpkg.Worker.process.
	source := []byte(`package mypkg

func Helper() {
	process()
}
`)
	p := NewGoParser()
	tree := p.Parse(source, nil)
	ext := &goExtractor{}

	nameToID := map[string]int64{
		"mypkg.Helper":            1,
		"otherpkg.Worker.process": 2,
	}

	edges := ext.Edges(tree, source, nameToID)

	requireNoEdge(t, edges, 1, 2)
}

func requireEdge(t *testing.T, edges []store.Edge, src, dst int64, callLine int) {
	t.Helper()
	for _, edge := range edges {
		if edge.Src == src && edge.Dst == dst && edge.Kind == store.EdgeCalls {
			require.Equal(t, callLine, edge.CallLine)
			return
		}
	}
	require.Failf(t, "missing edge", "%d -> %d not found in %+v", src, dst, edges)
}

func requireNoEdge(t *testing.T, edges []store.Edge, src, dst int64) {
	t.Helper()
	for _, edge := range edges {
		if edge.Src == src && edge.Dst == dst && edge.Kind == store.EdgeCalls {
			require.Failf(t, "unexpected edge", "%d -> %d found in %+v", src, dst, edges)
		}
	}
}
