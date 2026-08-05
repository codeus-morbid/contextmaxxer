package index

import "testing"

// Real shapes taken from the polyglot corpus, where the majority of captured
// "documentation" in PHP, C++ and TypeScript carried no meaning at all.
func TestNormalizeDocstringDropsAnnotationOnlyBlocks(t *testing.T) {
	noise := []struct{ name, doc string }{
		{"phpdoc params only", "/**\n * @param ContainerInterface|null $container\n */"},
		{"phpdoc param+return", "/**\n * @param ServerRequestInterface $request\n * @return ResponseInterface\n */"},
		{"ts internal marker", "/** @internal */"},
		{"clang-tidy suppression", "// NOLINT(hicpp-noexcept-move,performance-noexcept-move-constructor)"},
		{"doxygen tag only", "//! \\brief comparison operator"},
		{"license header", "// SPDX-License-Identifier: MIT"},
		{"markup leftovers", "/* {{{ */"},
	}
	for _, c := range noise {
		if got := NormalizeDocstring(c.doc); got != "" {
			t.Errorf("%s: expected nothing to survive, got %q", c.name, got)
		}
	}
}

func TestNormalizeDocstringKeepsProse(t *testing.T) {
	cases := []struct{ doc, want string }{
		{"// Sum adds two numbers.", "Sum adds two numbers."},
		{"/// Walks the directory tree and yields entries.", "Walks the directory tree and yields entries."},
		{
			"/**\n * Returns a decorator that applies all given decorators.\n * @param decorators one or more decorators\n */",
			"Returns a decorator that applies all given decorators.",
		},
		{
			"/**\n * @brief compare two values\n * Performs a lexicographic comparison of both operands.\n */",
			"Performs a lexicographic comparison of both operands.",
		},
	}
	for _, c := range cases {
		if got := NormalizeDocstring(c.doc); got != c.want {
			t.Errorf("NormalizeDocstring(%q) = %q, want %q", c.doc, got, c.want)
		}
	}
}
