package main

// -mode graph: reconstruct an approximate call graph from the benchmark's
// anonymous chunks and run the production PPR stage over the fused seeds.
//
// CORE-Bench ships no repository — only (id, text) chunks — so the pipeline
// stage that generic retrieval benchmarks cannot measure (personalized
// PageRank over the call graph) has nothing to run on. But the chunks ARE
// code: parsing each chunk with tree-sitter yields the symbols it defines
// and the calls it makes, which is enough to rebuild chunk->chunk edges
// with the same conservative unique-name discipline the indexer uses.
// Everything here is read-only over the embedding caches and built in
// memory per repo; nothing is persisted.

import (
	"context"
	"sort"

	"github.com/codeus-morbid/contextmaxxer/internal/benchdata"
	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/index/languages"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// graphLangs are tried during per-repo language detection, most common first.
var graphLangs = []string{
	"python", "typescript", "go", "java", "rust", "cpp", "c",
	"csharp", "ruby", "php", "kotlin", "scala",
}

type chunkGraph struct {
	graph *retrieve.Graph
	lang  string
	edges int
}

// detectLang samples chunks and picks the language whose extractor yields
// the most symbols. Doc/changelog chunks yield ~0 for every grammar and
// don't skew the vote.
func detectLang(parser index.Parser, docs []benchdata.CorpusDoc) string {
	sample := docs
	if len(sample) > 40 {
		step := len(docs) / 40
		sample = make([]benchdata.CorpusDoc, 0, 40)
		for i := 0; i < len(docs); i += step {
			sample = append(sample, docs[i])
		}
	}
	best, bestN := "", 0
	for _, lang := range graphLangs {
		ext, ok := languages.New(lang)
		if !ok {
			continue
		}
		n := 0
		for _, d := range sample {
			tree, err := parser.Parse(context.Background(), []byte(d.Text), lang)
			if err != nil || tree == nil {
				continue
			}
			n += len(ext.Symbols(tree, []byte(d.Text)))
		}
		if n > bestN {
			best, bestN = lang, n
		}
	}
	return best
}

// buildChunkGraph parses every chunk, maps qualified symbol names to their
// defining chunk (dropping names defined in more than one chunk — the same
// anti-hairball rule the indexer uses), then extracts call edges per chunk
// against that global map and collapses them to chunk<->chunk edges.
func buildChunkGraph(parser index.Parser, docs []benchdata.CorpusDoc) *chunkGraph {
	lang := detectLang(parser, docs)
	if lang == "" {
		return nil
	}
	ext, ok := languages.New(lang)
	if !ok {
		return nil
	}

	// Symbol id encodes the defining chunk: row*4096+k. Chunk row = id>>12.
	const symsPerChunk = 4096
	nameToID := make(map[string]int64)
	dup := make(map[string]bool)
	type parsed struct {
		row  int
		text []byte
	}
	var parsedChunks []parsed
	for row, d := range docs {
		src := []byte(d.Text)
		tree, err := parser.Parse(context.Background(), src, lang)
		if err != nil || tree == nil {
			continue
		}
		syms := ext.Symbols(tree, src)
		if len(syms) == 0 {
			continue
		}
		parsedChunks = append(parsedChunks, parsed{row: row, text: src})
		for k, s := range syms {
			if k >= symsPerChunk-1 {
				break
			}
			id := int64(row)*symsPerChunk + int64(k) + 1
			if _, seen := nameToID[s.QualifiedName]; seen {
				dup[s.QualifiedName] = true
				continue
			}
			nameToID[s.QualifiedName] = id
		}
	}
	// Names defined in several chunks are ambiguous — drop them entirely.
	for name := range dup {
		delete(nameToID, name)
	}

	// Second pass: call edges against the global map, collapsed to chunks.
	type pair struct{ a, b int64 }
	seen := map[pair]bool{}
	var edges []store.Edge
	for _, pc := range parsedChunks {
		tree, err := parser.Parse(context.Background(), pc.text, lang)
		if err != nil || tree == nil {
			continue
		}
		// Local symbol ids for THIS chunk must resolve too; they are already
		// in nameToID unless duplicated.
		for _, e := range ext.Edges(tree, pc.text, nameToID) {
			srcChunk := int64(pc.row)
			dstChunk := e.Dst / symsPerChunk
			if srcChunk == dstChunk {
				continue // intra-chunk edges add nothing to chunk ranking
			}
			p := pair{srcChunk, dstChunk}
			if seen[p] {
				continue
			}
			seen[p] = true
			edges = append(edges, store.Edge{Src: srcChunk, Dst: dstChunk})
		}
	}

	ids := make([]int64, len(docs))
	for i := range docs {
		ids[i] = int64(i)
	}
	return &chunkGraph{graph: retrieve.BuildGraph(ids, edges), lang: lang, edges: len(edges)}
}

// applyPPR blends the fused seed ranking with a personalized-PageRank pass
// over the chunk graph, mirroring the production pipeline stage:
// final = alpha*seed + (1-alpha)*ppr, both max-normalized over candidates.
func applyPPR(cg *chunkGraph, top []scoredRow, alpha float32, seedK int) []scoredRow {
	if cg == nil || cg.graph == nil || len(top) == 0 {
		return top
	}
	seeds := make(map[int64]float32, seedK)
	for i, sc := range top {
		if i >= seedK {
			break
		}
		seeds[int64(sc.row)] = sc.s
	}
	ranks, _ := retrieve.PersonalizedPageRank(cg.graph, seeds, 0.85, 30)

	var maxSeed, maxPPR float32
	for _, sc := range top {
		if sc.s > maxSeed {
			maxSeed = sc.s
		}
	}
	for _, sc := range top {
		if r := ranks[int64(sc.row)]; r > maxPPR {
			maxPPR = r
		}
	}
	if maxSeed == 0 || maxPPR == 0 {
		return top
	}

	out := make([]scoredRow, len(top))
	for i, sc := range top {
		blended := alpha*(sc.s/maxSeed) + (1-alpha)*(ranks[int64(sc.row)]/maxPPR)
		out[i] = scoredRow{row: sc.row, s: blended}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].s > out[j].s })
	return out
}
