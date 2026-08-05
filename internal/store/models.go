package store

const (
	KindFunction  = "function"
	KindMethod    = "method"
	KindClass     = "class"
	KindInterface = "interface"
	KindType      = "type"
	KindConst     = "const"
	KindVar       = "var"

	EdgeCalls   = "calls"
	EdgeImports = "imports"
)

type File struct {
	ID       int64
	Path     string
	Language string
	Hash     string
	Mtime    int64
	Size     int64
}

type Symbol struct {
	ID            int64
	FileID        int64
	Name          string
	Kind          string
	QualifiedName string
	StartLine     int
	EndLine       int
	Signature     string
	Docstring     string
	BodyExcerpt   string
}

// SymbolToEmbed is a symbol that has no stored embedding yet, joined with the
// file fields EmbeddingText needs. Produced by --fast (structure-only)
// indexing; consumed by the embedding backfill.
type SymbolToEmbed struct {
	Symbol
	Path     string
	Language string
}

type Edge struct {
	Src    int64
	Dst    int64
	Kind   string
	Weight float64
}

type ScoredSymbol struct {
	Symbol
	Score float32
}

type Embedding struct {
	SymbolID int64
	Vector   []float32
}
