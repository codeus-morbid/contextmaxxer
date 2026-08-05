package languages

import "github.com/codeus-morbid/contextmaxxer/internal/index"

func New(language string) (index.LanguageExtractor, bool) {
	switch language {
	case "go":
		return &goExtractor{}, true
	case "typescript", "javascript", "tsx":
		// tsx is a superset of typescript (adds JSX); the same extractor queries
		// (function/class/method declarations) match its parse tree. Closes the
		// React .tsx gap — without this case .tsx files were silently skipped.
		return &tsExtractor{}, true
	case "python":
		return &pyExtractor{}, true
	default:
		if cfg, ok := genericConfigs[language]; ok {
			return &genericExtractor{cfg: cfg}, true
		}
		return nil, false
	}
}
