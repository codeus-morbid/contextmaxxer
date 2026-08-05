package sqlite

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// vecCache keeps all symbol embeddings in RAM as one flat matrix.
// DECISION(2026-06): vec0 KNN took 150-640ms per query on the gen corpus
// (JSON-serialized query vector + virtual table scan). Project indexes are
// small (1-5k symbols x 768 dims = 3-15MB), so a brute-force dot product over
// an in-memory matrix answers the same query in single-digit milliseconds.
// vec0 remains the storage of record and the fallback path.
// REVISIT IF: indexes grow past ~100k symbols (then use a real ANN index).
type vecCache struct {
	mu     sync.RWMutex
	loaded bool
	dim    int
	ids    []int64
	mat    []float32 // row-major, len == len(ids)*dim
	// fileRemoved guards against re-deleting the sidecar on every mutation
	// during a large reindex (90k UpsertEmbedding calls would otherwise each
	// issue an os.Remove syscall).
	fileRemoved bool
}

const vecCacheMagic = "CTXVEC01"

func (s *Store) vecCachePath() string { return s.path + ".veccache" }

func (s *Store) invalidateVecCache() {
	// Symbols/edges mutate through the same write paths as vectors, so the
	// graph cache rides the same invalidation hooks.
	s.invalidateGraphCache()
	s.vec.mu.Lock()
	s.vec.loaded = false
	s.vec.ids = nil
	s.vec.mat = nil
	// A mutated index must not be served from a stale sidecar; drop it so the
	// next load rebuilds from the DB (and re-materializes a fresh sidecar).
	if !s.vec.fileRemoved && s.path != "" {
		_ = os.Remove(s.vecCachePath())
		s.vec.fileRemoved = true
	}
	s.vec.mu.Unlock()
}

// ensureVecCache loads all embeddings into RAM on first use. It prefers the
// on-disk sidecar (one sequential read) when it matches the current index
// generation token, falling back to a full symbol_vec scan and materializing
// the sidecar for next time.
func (s *Store) ensureVecCache(ctx context.Context) error {
	s.vec.mu.RLock()
	if s.vec.loaded {
		s.vec.mu.RUnlock()
		return nil
	}
	s.vec.mu.RUnlock()

	s.vec.mu.Lock()
	defer s.vec.mu.Unlock()
	if s.vec.loaded {
		return nil
	}

	token := s.vecToken()
	if token != "" {
		if ids, mat, dim, ok := s.loadVecCacheFile(token); ok {
			s.vec.ids, s.vec.mat, s.vec.dim = ids, mat, dim
			s.vec.loaded, s.vec.fileRemoved = true, false
			return nil
		}
	}

	ids, mat, dim, err := s.loadVecFromDB(ctx)
	if err != nil {
		return err
	}
	s.vec.ids, s.vec.mat, s.vec.dim, s.vec.loaded = ids, mat, dim, true

	// Materialize the sidecar for next time. Best-effort: a read path must not
	// fail because the cache file couldn't be written.
	if token != "" {
		if err := s.writeVecCacheFile(token, ids, mat, dim); err == nil {
			s.vec.fileRemoved = false
		}
	}
	return nil
}

// loadVecFromDB scans symbol_vec into a flat RAM matrix (the slow cold path).
func (s *Store) loadVecFromDB(ctx context.Context) (ids []int64, mat []float32, dim int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT symbol_id, embedding FROM symbol_vec`)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("vec cache load: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, nil, 0, fmt.Errorf("vec cache scan: %w", err)
		}
		if len(raw)%4 != 0 {
			return nil, nil, 0, fmt.Errorf("vec cache: invalid embedding length %d for symbol %d", len(raw), id)
		}
		n := len(raw) / 4
		if dim == 0 {
			dim = n
		} else if n != dim {
			return nil, nil, 0, fmt.Errorf("vec cache: dimension mismatch %d != %d for symbol %d", n, dim, id)
		}
		ids = append(ids, id)
		for i := 0; i < n; i++ {
			mat = append(mat, math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:i*4+4])))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("vec cache rows: %w", err)
	}
	return ids, mat, dim, nil
}

// RebuildVecCacheFile bumps the index generation token, loads the embedding
// matrix from the DB, and writes the on-disk sidecar. Called at the end of
// indexing so the first query after an index is fast (no cold 90k-row load).
func (s *Store) RebuildVecCacheFile(ctx context.Context) error {
	token, err := s.bumpVecToken()
	if err != nil {
		return err
	}
	ids, mat, dim, err := s.loadVecFromDB(ctx)
	if err != nil {
		return err
	}
	s.vec.mu.Lock()
	s.vec.ids, s.vec.mat, s.vec.dim, s.vec.loaded = ids, mat, dim, true
	s.vec.mu.Unlock()
	if err := s.writeVecCacheFile(token, ids, mat, dim); err != nil {
		return err
	}
	s.vec.mu.Lock()
	s.vec.fileRemoved = false
	s.vec.mu.Unlock()
	return nil
}

// vecToken returns the current index generation token from _meta, or "".
func (s *Store) vecToken() string {
	var tok string
	_ = s.db.QueryRow(`SELECT value FROM _meta WHERE key='vec_token'`).Scan(&tok)
	return tok
}

// bumpVecToken writes a fresh generation token to _meta and returns it. Every
// reindex changes the token, which invalidates any sidecar tagged with the old.
func (s *Store) bumpVecToken() (string, error) {
	tok := strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO _meta(key,value) VALUES('vec_token',?)`, tok); err != nil {
		return "", fmt.Errorf("bump vec token: %w", err)
	}
	return tok, nil
}

// loadVecCacheFile reads the sidecar and returns its matrix iff the file is
// well-formed and tagged with the expected token. ok=false means "fall back to
// the DB" — never an error to the caller.
func (s *Store) loadVecCacheFile(expectToken string) (ids []int64, mat []float32, dim int, ok bool) {
	if s.path == "" {
		return nil, nil, 0, false
	}
	f, err := os.Open(s.vecCachePath())
	if err != nil {
		return nil, nil, 0, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	magic := make([]byte, len(vecCacheMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != vecCacheMagic {
		return nil, nil, 0, false
	}
	var hdr [16]byte // dim(int32) count(int64) tokenLen(int32)
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, nil, 0, false
	}
	dim = int(int32(binary.LittleEndian.Uint32(hdr[0:4])))
	count := int64(binary.LittleEndian.Uint64(hdr[4:12]))
	tokenLen := int(int32(binary.LittleEndian.Uint32(hdr[12:16])))
	if dim <= 0 || count < 0 || tokenLen < 0 || tokenLen > 1024 {
		return nil, nil, 0, false
	}
	if s.dim != 0 && dim != s.dim {
		return nil, nil, 0, false
	}
	tok := make([]byte, tokenLen)
	if _, err := io.ReadFull(r, tok); err != nil || string(tok) != expectToken {
		return nil, nil, 0, false
	}

	idBytes := make([]byte, count*8)
	if _, err := io.ReadFull(r, idBytes); err != nil {
		return nil, nil, 0, false
	}
	matBytes := make([]byte, count*int64(dim)*4)
	if _, err := io.ReadFull(r, matBytes); err != nil {
		return nil, nil, 0, false
	}

	ids = make([]int64, count)
	for i := range ids {
		ids[i] = int64(binary.LittleEndian.Uint64(idBytes[i*8 : i*8+8]))
	}
	mat = make([]float32, count*int64(dim))
	for i := range mat {
		mat[i] = math.Float32frombits(binary.LittleEndian.Uint32(matBytes[i*4 : i*4+4]))
	}
	return ids, mat, dim, true
}

// writeVecCacheFile writes the sidecar atomically (temp + rename).
func (s *Store) writeVecCacheFile(token string, ids []int64, mat []float32, dim int) error {
	if s.path == "" {
		return fmt.Errorf("vec cache: no index path")
	}
	if len(mat) != len(ids)*dim {
		return fmt.Errorf("vec cache: matrix shape %d != %d*%d", len(mat), len(ids), dim)
	}
	tmp := s.vecCachePath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("vec cache create: %w", err)
	}
	w := bufio.NewWriterSize(f, 1<<20)

	var hdr [16]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(int32(dim)))
	binary.LittleEndian.PutUint64(hdr[4:12], uint64(len(ids)))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(int32(len(token))))
	var buf [8]byte
	writeErr := func() error {
		if _, err := w.WriteString(vecCacheMagic); err != nil {
			return err
		}
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.WriteString(token); err != nil {
			return err
		}
		for _, id := range ids {
			binary.LittleEndian.PutUint64(buf[:], uint64(id))
			if _, err := w.Write(buf[:]); err != nil {
				return err
			}
		}
		for _, v := range mat {
			binary.LittleEndian.PutUint32(buf[:4], math.Float32bits(v))
			if _, err := w.Write(buf[:4]); err != nil {
				return err
			}
		}
		return w.Flush()
	}()
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if cerr := f.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vec cache write: %w", writeErr)
	}
	if err := os.Rename(tmp, s.vecCachePath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("vec cache rename: %w", err)
	}
	return nil
}

type vecHit struct {
	id    int64
	score float32
}

// searchVecCache returns the top-k symbol ids with scores computed exactly as
// the vec0 path did: score = 1/(1+L2distance). Embeddings are L2-normalized,
// so L2distance = sqrt(2-2*dot).
func (s *Store) searchVecCache(query []float32, k int) ([]vecHit, bool) {
	s.vec.mu.RLock()
	defer s.vec.mu.RUnlock()
	if !s.vec.loaded || s.vec.dim == 0 || len(s.vec.ids) == 0 || len(query) != s.vec.dim {
		return nil, false
	}

	hits := make([]vecHit, len(s.vec.ids))
	dim := s.vec.dim
	for row := range s.vec.ids {
		base := row * dim
		var dot float32
		vec := s.vec.mat[base : base+dim]
		for i, q := range query {
			dot += q * vec[i]
		}
		d2 := 2 - 2*dot
		if d2 < 0 {
			d2 = 0
		}
		dist := float32(math.Sqrt(float64(d2)))
		hits[row] = vecHit{id: s.vec.ids[row], score: 1 / (1 + dist)}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if k < len(hits) {
		hits = hits[:k]
	}
	return hits, true
}
