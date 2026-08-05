package sqlite

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

func normalized(dim int, seed float32) []float32 {
	vec := make([]float32, dim)
	var sum float64
	for i := range vec {
		vec[i] = seed + float32(i%7)*0.01
		sum += float64(vec[i]) * float64(vec[i])
	}
	norm := float32(math.Sqrt(sum))
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

func setupVecStore(t *testing.T) (*Store, []int64, [][]float32) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()

	fid, err := s.SaveFile(ctx, &store.File{Path: "/v.go", Language: "go", Hash: "h", Mtime: 0})
	require.NoError(t, err)

	ids, err := s.SaveSymbolBatch(ctx, []store.Symbol{
		{FileID: fid, Name: "A", Kind: "func", QualifiedName: "pkg.A", BodyExcerpt: "a"},
		{FileID: fid, Name: "B", Kind: "func", QualifiedName: "pkg.B", BodyExcerpt: "b"},
		{FileID: fid, Name: "C", Kind: "func", QualifiedName: "pkg.C", BodyExcerpt: "c"},
	})
	require.NoError(t, err)

	vecs := [][]float32{normalized(384, 0.1), normalized(384, 0.5), normalized(384, 2.0)}
	for i, id := range ids {
		require.NoError(t, s.UpsertEmbedding(ctx, id, vecs[i]))
	}
	return s, ids, vecs
}

func TestVecCache_MatchesVec0Path(t *testing.T) {
	s, _, vecs := setupVecStore(t)
	ctx := context.Background()
	query := vecs[1]

	cached, err := s.SearchByVectorScored(ctx, query, 3)
	require.NoError(t, err)
	require.Len(t, cached, 3)

	direct, err := s.searchByVectorScoredVec0(ctx, query, 3)
	require.NoError(t, err)
	require.Len(t, direct, 3)

	for i := range cached {
		require.Equal(t, direct[i].QualifiedName, cached[i].QualifiedName, "rank %d order mismatch", i)
		require.InDelta(t, direct[i].Score, cached[i].Score, 1e-4, "rank %d score mismatch", i)
	}
	require.Equal(t, "pkg.B", cached[0].QualifiedName, "query equals B's vector, B must rank first")
}

func TestVecCache_InvalidatedByUpsert(t *testing.T) {
	s, ids, vecs := setupVecStore(t)
	ctx := context.Background()
	query := vecs[2]

	first, err := s.SearchByVectorScored(ctx, query, 1)
	require.NoError(t, err)
	require.Equal(t, "pkg.C", first[0].QualifiedName)

	// Move A onto the query vector; cache must be refreshed and A must win
	// (ties broken deterministically is not required — A score must be ~1.0).
	require.NoError(t, s.UpsertEmbedding(ctx, ids[0], vecs[2]))
	second, err := s.SearchByVectorScored(ctx, query, 2)
	require.NoError(t, err)
	// float32 self-dot of a unit vector lands slightly below 1.0.
	require.InDelta(t, 1.0, second[0].Score, 5e-3, "exact match must score ~1.0 after cache refresh")
	names := []string{second[0].QualifiedName, second[1].QualifiedName}
	require.Contains(t, names, "pkg.A")
	require.Contains(t, names, "pkg.C")
}

func TestVecCache_SidecarServesWhenDBVectorsGone(t *testing.T) {
	s, _, vecs := setupVecStore(t)
	ctx := context.Background()

	// Pre-build the sidecar (bumps the token + writes the file).
	require.NoError(t, s.RebuildVecCacheFile(ctx))
	require.FileExists(t, s.vecCachePath())

	// Wipe the DB vectors but keep the sidecar and token. If search still works,
	// the matrix MUST have been loaded from the sidecar, not the DB.
	_, err := s.db.Exec(`DELETE FROM symbol_vec`)
	require.NoError(t, err)
	s.vec.mu.Lock()
	s.vec.loaded, s.vec.ids, s.vec.mat = false, nil, nil
	s.vec.mu.Unlock()

	res, err := s.SearchByVectorScored(ctx, vecs[1], 3)
	require.NoError(t, err)
	require.Len(t, res, 3, "results must come from the sidecar, not the wiped DB")
	require.Equal(t, "pkg.B", res[0].QualifiedName, "query equals B's vector")
}

func TestVecCache_SidecarInvalidatedOnMutationAndTokenMismatch(t *testing.T) {
	s, ids, vecs := setupVecStore(t)
	ctx := context.Background()

	require.NoError(t, s.RebuildVecCacheFile(ctx))
	require.FileExists(t, s.vecCachePath())

	// An embedding mutation must remove the sidecar so a stale generation can't
	// be served.
	require.NoError(t, s.UpsertEmbedding(ctx, ids[0], vecs[2]))
	_, statErr := os.Stat(s.vecCachePath())
	require.True(t, os.IsNotExist(statErr), "sidecar must be removed after an embedding mutation")

	// A sidecar tagged with a stale token is ignored (defends against a swapped db).
	require.NoError(t, s.RebuildVecCacheFile(ctx))
	require.NoError(t, s.writeVecCacheFile("stale-token", []int64{ids[0]}, make([]float32, 384), 384))
	s.vec.mu.Lock()
	s.vec.loaded, s.vec.ids, s.vec.mat = false, nil, nil
	s.vec.mu.Unlock()
	res, err := s.SearchByVectorScored(ctx, vecs[1], 3)
	require.NoError(t, err)
	require.Len(t, res, 3, "stale-token sidecar must be ignored and the DB used")
}

func TestVecCache_InvalidatedByDeleteSymbolsByFile(t *testing.T) {
	s, _, vecs := setupVecStore(t)
	ctx := context.Background()

	res, err := s.SearchByVectorScored(ctx, vecs[0], 3)
	require.NoError(t, err)
	require.Len(t, res, 3)

	f, err := s.GetFileByPath(ctx, "/v.go")
	require.NoError(t, err)
	require.NoError(t, s.DeleteSymbolsByFile(ctx, f.ID))

	res, err = s.SearchByVectorScored(ctx, vecs[0], 3)
	require.NoError(t, err)
	require.Empty(t, res)
}
