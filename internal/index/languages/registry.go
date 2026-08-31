package languages

import "github.com/codeus-morbid/contextmaxxer/internal/index"

func New(language string) (index.LanguageExtractor, bool) {
	switch language {
	case "go":
		return &goExtractor{}, true
	case "typescript":
		return &tsExtractor{}, true
	case "javascript", "tsx":
		// The same extractor handles all three, but the parser gives these two a
		// TSX tree (TSX is a superset of typescript that also parses JSX), and
		// the extractor's call query has to be compiled against that grammar to
		// match anything. See tsExtractor.
		return &tsExtractor{tsx: true}, true
	case "python":
		return &pyExtractor{}, true
	default:
		if cfg, ok := genericConfigs[language]; ok {
			return &genericExtractor{cfg: cfg}, true
		}
		return nil, false
	}
}
