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
	requireNoEdge(t, ambiguous, 1, 2)
	requireNoEdge(t, ambiguous, 1, 3)

	// Unique: a single "save" suffix resolves to that one symbol.
	unique := ext.Edges(tree, source, map[string]int64{"run": 1, "A.save": 2})
	requireEdge(t, unique, 1, 2, 2)
}

func TestPyExtractor_SelfCallBindsToOwnClass(t *testing.T) {
	// `self.clean()` where "clean" also exists on another class: the global
	// unique-suffix guard drops it, so the receiver has to decide.
	source := []byte("class A:\n    def clean(self, v):\n        return v\n\n    def run(self, v):\n        return self.clean(v)\n\nclass B:\n    def clean(self, v):\n        return v\n")
	p := NewPyParser()
	tree := p.Parse(source, nil)
	ext := &pyExtractor{}

	nameToID := map[string]int64{"A": 1, "A.clean": 2, "A.run": 3, "B": 4, "B.clean": 5}
	edges := ext.Edges(tree, source, nameToID)

	requireEdge(t, edges, 3, 2, 6)
	requireNoEdge(t, edges, 3, 5)
}

func TestPyExtractor_SelfCallWalksBaseClasses(t *testing.T) {
	// Django's WSGIHandler.__call__ -> BaseHandler.get_response: the method is
	// inherited, and the name repeats, so only the base walk finds it.
	source := []byte("class Base:\n    def get_response(self, r):\n        return r\n\nclass Handler(Base):\n    def __call__(self, r):\n        return self.get_response(r)\n\nclass Other:\n    def get_response(self, r):\n        return r\n")
	p := NewPyParser()
	tree := p.Parse(source, nil)
	ext := &pyExtractor{}

	nameToID := map[string]int64{
		"Base": 1, "Base.get_response": 2,
		"Handler": 3, "Handler.__call__": 4,
		"Other": 5, "Other.get_response": 6,
	}
	edges := ext.Edges(tree, source, nameToID)

	requireEdge(t, edges, 4, 2, 7)
	requireNoEdge(t, edges, 4, 6)
}

func TestPyExtractor_ForeignReceiverStaysConservative(t *testing.T) {
	// Only `self`/`cls` is a known type. An arbitrary receiver must still fall
	// through to the unique-suffix guard, or the Django hairball comes back.
	source := []byte("def run(bf):\n    return bf.clean(1)\n")
	p := NewPyParser()
	tree := p.Parse(source, nil)
	ext := &pyExtractor{}

	edges := ext.Edges(tree, source, map[string]int64{"run": 1, "A.clean": 2, "B.clean": 3})
	requireNoEdge(t, edges, 1, 2)
	requireNoEdge(t, edges, 1, 3)
}

func TestPyExtractor_SelfCallOverrideWinsOverBase(t *testing.T) {
	source := []byte("class Base:\n    def clean(self, v):\n        return v\n\nclass Child(Base):\n    def clean(self, v):\n        return v\n\n    def run(self, v):\n        return self.clean(v)\n")
	p := NewPyParser()
	tree := p.Parse(source, nil)
	ext := &pyExtractor{}

	nameToID := map[string]int64{"Base": 1, "Base.clean": 2, "Child": 3, "Child.clean": 4, "Child.run": 5}
	edges := ext.Edges(tree, source, nameToID)

	requireEdge(t, edges, 5, 4, 10)
	requireNoEdge(t, edges, 5, 2)
}
