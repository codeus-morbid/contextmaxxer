package index

import (
	"context"
	"fmt"

	tree_sitter_kotlin "github.com/tree-sitter-grammars/tree-sitter-kotlin/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_c_sharp "github.com/tree-sitter/tree-sitter-c-sharp/bindings/go"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_php "github.com/tree-sitter/tree-sitter-php/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_scala "github.com/tree-sitter/tree-sitter-scala/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

type Parser interface {
	Parse(ctx context.Context, source []byte, language string) (*tree_sitter.Tree, error)
	LanguageNames() []string
	Close() error
}

type treeSitterParser struct {
	languages map[string]*tree_sitter.Language
}

func NewTreeSitterParser() (Parser, error) {
	// DECISION: languages are initialized once at construction and reused across calls;
	// parsers are created per-call to avoid concurrency issues with TSParser state.
	langs := map[string]*tree_sitter.Language{
		"go":         tree_sitter.NewLanguage(tree_sitter_go.Language()),
		"typescript": tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript()),
		"tsx":        tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()),
		// DECISION: javascript reuses tsx grammar because tsx is a superset that parses JSX too.
		"javascript": tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX()),
		"python":     tree_sitter.NewLanguage(tree_sitter_python.Language()),
		// Tier-2 languages (symbols-only extraction; see languages/generic.go).
		"java":   tree_sitter.NewLanguage(tree_sitter_java.Language()),
		"rust":   tree_sitter.NewLanguage(tree_sitter_rust.Language()),
		"c":      tree_sitter.NewLanguage(tree_sitter_c.Language()),
		"cpp":    tree_sitter.NewLanguage(tree_sitter_cpp.Language()),
		"csharp": tree_sitter.NewLanguage(tree_sitter_c_sharp.Language()),
		"ruby":   tree_sitter.NewLanguage(tree_sitter_ruby.Language()),
		"php":    tree_sitter.NewLanguage(tree_sitter_php.LanguagePHP()),
		"kotlin": tree_sitter.NewLanguage(tree_sitter_kotlin.Language()),
		"scala":  tree_sitter.NewLanguage(tree_sitter_scala.Language()),
	}
	return &treeSitterParser{languages: langs}, nil
}

func (p *treeSitterParser) Parse(_ context.Context, source []byte, language string) (*tree_sitter.Tree, error) {
	lang, ok := p.languages[language]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %q", language)
	}

	parser := tree_sitter.NewParser()
	defer parser.Close()

	if err := parser.SetLanguage(lang); err != nil {
		return nil, fmt.Errorf("set language %q: %w", language, err)
	}

	tree := parser.Parse(source, nil)
	return tree, nil
}

func (p *treeSitterParser) LanguageNames() []string {
	names := make([]string, 0, len(p.languages))
	for name := range p.languages {
		names = append(names, name)
	}
	return names
}

func (p *treeSitterParser) Close() error {
	return nil
}
