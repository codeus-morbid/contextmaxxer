package store

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("store: not found")

type Store interface {
	SaveFile(ctx context.Context, f *File) (int64, error)
	GetFileByPath(ctx context.Context, path string) (*File, error)
	DeleteFile(ctx context.Context, id int64) error
	ListFiles(ctx context.Context) ([]File, error)

	SaveSymbol(ctx context.Context, s *Symbol) (int64, error)
	SaveSymbolBatch(ctx context.Context, symbols []Symbol) ([]int64, error)
	DeleteSymbolsByFile(ctx context.Context, fileID int64) error
	ListSymbolsByFile(ctx context.Context, fileID int64) ([]Symbol, error)
	ListSymbolsByLanguage(ctx context.Context, language string) ([]Symbol, error)

	SaveEdge(ctx context.Context, e Edge) error
	SaveEdgeBatch(ctx context.Context, edges []Edge) error

	UpsertEmbedding(ctx context.Context, symbolID int64, vec []float32) error
	UpsertEmbeddingBatch(ctx context.Context, embeddings []Embedding) error
	SaveSymbolChunks(ctx context.Context, chunks []SymbolChunk) ([]int64, error)
	UpsertChunkEmbeddingBatch(ctx context.Context, embeddings []ChunkEmbedding) error
	GetEmbedding(ctx context.Context, symbolID int64) ([]float32, bool, error)

	SearchByVector(ctx context.Context, embedding []float32, topK int) ([]Symbol, error)
	GetEdges(ctx context.Context, symbolID int64) ([]Edge, error)

	SearchByVectorScored(ctx context.Context, vec []float32, k int) ([]ScoredSymbol, error)
	SearchByText(ctx context.Context, query string, k int) ([]ScoredSymbol, error)
	GetSymbolsByIDs(ctx context.Context, ids []int64) ([]Symbol, error)
	GetFilesByIDs(ctx context.Context, ids []int64) (map[int64]string, error)
	ListAllEdges(ctx context.Context) ([]Edge, error)
	ListAllSymbolIDs(ctx context.Context) ([]int64, error)
	ListSymbolMeta(ctx context.Context) ([]Symbol, error)
	GetEmbeddingsByIDs(ctx context.Context, ids []int64) (map[int64][]float32, error)
	GetCallerEdges(ctx context.Context, dstIDs []int64, limitPerSymbol int) (map[int64][]int64, error)
	GetCalleeEdges(ctx context.Context, srcIDs []int64, limitPerSymbol int) (map[int64][]int64, error)

	Close() error
}
