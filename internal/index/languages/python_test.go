package languages

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const pySource = `
def helper():
    """Utility function."""
    pass

class MyClass:
    """A sample class."""

    def process(self):
        helper()
`

func TestPyExtractor_Symbols(t *testing.T) {
	p := NewPyParser()
	tree := p.Parse([]byte(pySource), nil)
	require.NotNil(t, tree)

	ext := &pyExtractor{}
	syms := ext.Symbols(tree, []byte(pySource))

	byName := make(map[string]string)
	for _, s := range syms {
		byName[s.Name] = s.Kind
	}

	require.Equal(t, "function", byName["helper"])
	require.Equal(t, "class", byName["MyClass"])
	require.Equal(t, "method", byName["process"])
}

func TestPyExtractor_QualifiedNames(t *testing.T) {
	p := NewPyParser()
	tree := p.Parse([]byte(pySource), nil)
	ext := &pyExtractor{}
	syms := ext.Symbols(tree, []byte(pySource))

	byQName := make(map[string]bool)
	for _, s := range syms {
		byQName[s.QualifiedName] = true
	}

	require.True(t, byQName["MyClass.process"])
	require.True(t, byQName["helper"])
	require.True(t, byQName["MyClass"])
}

func TestPyExtractor_Docstring(t *testing.T) {
	p := NewPyParser()
	tree := p.Parse([]byte(pySource), nil)
	ext := &pyExtractor{}
	syms := ext.Symbols(tree, []byte(pySource))

	for _, s := range syms {
		if s.Name == "helper" {
			require.Contains(t, s.Docstring, "Utility function")
			return
		}
	}
	t.Fatal("helper not found")
}

func TestPyExtractor_Edges(t *testing.T) {
	p := NewPyParser()
	tree := p.Parse([]byte(pySource), nil)
	ext := &pyExtractor{}
	syms := ext.Symbols(tree, []byte(pySource))

	nameToID := make(map[string]int64)
	for i, s := range syms {
		nameToID[s.QualifiedName] = int64(i + 1)
	}

	edges := ext.Edges(tree, []byte(pySource), nameToID)

	processID := nameToID["MyClass.process"]
	helperID := nameToID["helper"]
	require.NotZero(t, processID)
	require.NotZero(t, helperID)

	found := false
	for _, e := range edges {
		if e.Src == processID && e.Dst == helperID && e.Kind == "calls" {
			found = true
			break
		}
	}
	require.True(t, found, "expected edge process -> helper, edges: %+v", edges)
}

func TestPyExtractor_EdgesUniqueSuffixOnly(t *testing.T) {
	source := []byte("def run():\n    save()\n")
	p := NewPyParser()
	tree := p.Parse(source, nil)
	ext := &pyExtractor{}

	// Ambiguous: two symbols share the suffix "save" -> no edge (Django's hundreds
	// of same-named methods otherwise exploded the graph to ~35 edges/symbol).
	ambiguous := ext.Edges(tree, source, map[string]int64{"run": 1, "A.save": 2, "B.save": 3})
	require.NotContains(t, ambiguous, edge(1, 2), "ambiguous suffix must not fan out: %+v", ambiguous)
	require.NotContains(t, ambiguous, edge(1, 3))

	// Unique: a single "save" suffix resolves to that one symbol.
	unique := ext.Edges(tree, source, map[string]int64{"run": 1, "A.save": 2})
	require.Contains(t, unique, edge(1, 2), "unique suffix must resolve: %+v", unique)
}
