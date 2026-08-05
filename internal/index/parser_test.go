package index

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestParser(t *testing.T) Parser {
	t.Helper()
	p, err := NewTreeSitterParser()
	require.NoError(t, err)
	return p
}

func TestParser_ParsesGo(t *testing.T) {
	p := newTestParser(t)
	defer p.Close()

	src := []byte("package x; func Foo() {}")
	tree, err := p.Parse(context.Background(), src, "go")
	require.NoError(t, err)
	require.NotNil(t, tree)

	root := tree.RootNode()
	require.Equal(t, "source_file", root.Kind())
	require.False(t, root.HasError())
}

func TestParser_ParsesTypeScript(t *testing.T) {
	p := newTestParser(t)
	defer p.Close()

	src := []byte("function foo(): number { return 1; }")
	tree, err := p.Parse(context.Background(), src, "typescript")
	require.NoError(t, err)
	require.NotNil(t, tree)

	root := tree.RootNode()
	require.False(t, root.HasError())
}

func TestParser_ParsesPython(t *testing.T) {
	p := newTestParser(t)
	defer p.Close()

	src := []byte("def foo(): pass")
	tree, err := p.Parse(context.Background(), src, "python")
	require.NoError(t, err)
	require.NotNil(t, tree)

	root := tree.RootNode()
	require.False(t, root.HasError())
}

func TestParser_UnknownLanguage(t *testing.T) {
	p := newTestParser(t)
	defer p.Close()

	_, err := p.Parse(context.Background(), []byte("anything"), "cobol")
	require.Error(t, err)
}
