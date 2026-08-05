// Package benchdata loads BEIR-format retrieval benchmark data (the layout
// CORE-Bench ships: per-repo corpus.jsonl / queries.jsonl / qrels/test.tsv)
// and manages the on-disk embedding-matrix cache shared by cmd/corebench and
// cmd/ftdata.
package benchdata

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type CorpusDoc struct {
	ID   string `json:"_id"`
	Text string `json:"text"`
}

type Query struct {
	ID   string `json:"_id"`
	Text string `json:"text"`
	// Filtered is the benchmark's temporal filter: the corpus ids that
	// existed at the query's repo snapshot. Empty = whole corpus eligible.
	Filtered []string `json:"filtered_corpus_id"`
}

func LoadCorpus(path string, maxChars int) ([]CorpusDoc, error) {
	var docs []CorpusDoc
	err := ReadJSONL(path, func(line []byte) error {
		var d CorpusDoc
		if err := json.Unmarshal(line, &d); err != nil {
			return err
		}
		if maxChars > 0 && len(d.Text) > maxChars {
			d.Text = d.Text[:maxChars]
		}
		docs = append(docs, d)
		return nil
	})
	return docs, err
}

func LoadQueries(path string, maxChars int) ([]Query, error) {
	var qs []Query
	err := ReadJSONL(path, func(line []byte) error {
		var q Query
		if err := json.Unmarshal(line, &q); err != nil {
			return err
		}
		if maxChars > 0 && len(q.Text) > maxChars {
			q.Text = q.Text[:maxChars]
		}
		qs = append(qs, q)
		return nil
	})
	return qs, err
}

// LoadQrels reads a BEIR qrels TSV (query-id, corpus-id, score header line).
func LoadQrels(path string) (map[string]map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rel := map[string]map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 3 {
			continue
		}
		var score int
		fmt.Sscanf(parts[2], "%d", &score)
		if rel[parts[0]] == nil {
			rel[parts[0]] = map[string]int{}
		}
		rel[parts[0]][parts[1]] = score
	}
	return rel, sc.Err()
}

func ReadJSONL(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// LoadMatrix reads a cached row-major float32 embedding matrix written by
// SaveMatrix, validating the (count, dim) header against expectations.
func LoadMatrix(path string, count, dim int) ([]float32, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var hdr [2]int32
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil || int(hdr[0]) != count || int(hdr[1]) != dim {
		return nil, false
	}
	mat := make([]float32, count*dim)
	if err := binary.Read(bufio.NewReaderSize(f, 1<<20), binary.LittleEndian, &mat); err != nil {
		return nil, false
	}
	return mat, true
}

// PeekMatrix reads only the (count, dim) header, so callers can validate a
// cache without loading gigabytes.
func PeekMatrix(path string) (count, dim int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	var hdr [2]int32
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil {
		return 0, 0, false
	}
	return int(hdr[0]), int(hdr[1]), true
}

func SaveMatrix(path string, mat []float32, count, dim int) error {
	if len(mat) != count*dim {
		return fmt.Errorf("matrix shape %d != %d*%d", len(mat), count, dim)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if err := binary.Write(w, binary.LittleEndian, [2]int32{int32(count), int32(dim)}); err == nil {
		err = binary.Write(w, binary.LittleEndian, mat)
	}
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// Dot is the similarity between two L2-normalized vectors.
func Dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// Sanitize turns a repo key into a filesystem-safe cache-file stem.
func Sanitize(s string) string {
	return strings.NewReplacer("/", "_", "\\", "_").Replace(s)
}
