package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"time"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type Stats struct {
	FilesIndexed int
	FilesSkipped int
	FilesDeleted int
	Symbols      int
	Edges        int
	Duration     time.Duration
}

// FileWalker is the walker interface consumed by Indexer.
type FileWalker interface {
	Walk(ctx context.Context, root string) (<-chan FileRecord, <-chan error)
}

type knownFilesWalker interface {
	SetKnownFiles(map[string]store.File)
}

// ExtractorFactory returns a LanguageExtractor for the given language name.
// The bool indicates whether the language is supported.
type ExtractorFactory func(language string) (LanguageExtractor, bool)

type Indexer struct {
	store     store.Store
	parser    Parser
	embedder  embed.Embedder
	walker    FileWalker
	extractor ExtractorFactory
	// docOverlay maps qualified symbol names to synthetic purpose summaries
	// merged into Docstring at index time (offline-generated, e.g. by an LLM).
	docOverlay   map[string]string
	forceReindex bool
	log          *slog.Logger
}

// SetForceReindex disables the unchanged-file hash skip so every file is
// re-parsed and re-embedded. Needed after the doc overlay changes: file
// hashes stay the same but embedding texts do not. Per-symbol embedding reuse
// still applies, so only symbols whose text actually changed hit the embedder.
func (idx *Indexer) SetForceReindex(force bool) {
	idx.forceReindex = force
}

// SetDocOverlay installs synthetic docstrings merged into symbols during
// indexing. DECISION(2026-06): purpose summaries close the vocabulary gap
// between intent-style queries ("why") and code text ("how") — the dominant
// paraphrastic failure on the gen corpus. The summary is prepended to any
// existing docstring so it leads the embedding text and the reranker document.
// REVISIT IF: paraphrastic slice does not improve after overlay reindex.
func (idx *Indexer) SetDocOverlay(m map[string]string) {
	idx.docOverlay = m
}

func NewIndexer(s store.Store, p Parser, e embed.Embedder, w FileWalker, ef ExtractorFactory, log *slog.Logger) *Indexer {
	return &Indexer{store: s, parser: p, embedder: e, walker: w, extractor: ef, log: log}
}

type edgeExtractionWork struct {
	tree      *tree_sitter.Tree
	source    []byte
	language  string
	extractor LanguageExtractor
}

// DECISION: file processing is sequential (single goroutine consuming the walker channel).
// SQLite has a single-writer constraint, so parallelism here would require a work queue +
// serialized writes anyway. Add bounded parallelism for parse+extract if profiling shows it matters.
func (idx *Indexer) Index(ctx context.Context, root string) (Stats, error) {
	start := time.Now()
	var stats Stats

	existingList, err := idx.store.ListFiles(ctx)
	if err != nil {
		return stats, err
	}
	existing := make(map[string]*store.File, len(existingList))
	for i := range existingList {
		f := &existingList[i]
		existing[f.Path] = f
	}
	if w, ok := idx.walker.(knownFilesWalker); ok {
		known := make(map[string]store.File, len(existingList))
		for _, f := range existingList {
			known[f.Path] = f
		}
		w.SetKnownFiles(known)
	}

	seen := make(map[string]bool, len(existing))
	runNameToIDByLang := make(map[string]map[string]int64)
	loadedLangMaps := make(map[string]bool)
	var edgeWork []edgeExtractionWork

	records, errs := idx.walker.Walk(ctx, root)

	for rec := range records {
		if prev, ok := existing[rec.RelPath]; ok && prev.Hash == rec.Hash && !idx.forceReindex {
			seen[rec.RelPath] = true
			stats.FilesSkipped++
			continue
		}

		source, err := os.ReadFile(rec.Path)
		if err != nil {
			idx.log.Warn("read file error", "path", rec.Path, "err", err)
			continue
		}

		tree, err := idx.parser.Parse(ctx, source, rec.Language)
		if err != nil {
			idx.log.Warn("parse error", "path", rec.Path, "lang", rec.Language, "err", err)
			continue
		}

		extractor, ok := idx.extractor(rec.Language)
		if !ok {
			idx.log.Warn("no extractor for language", "lang", rec.Language, "path", rec.Path)
			continue
		}

		if !loadedLangMaps[rec.Language] {
			syms, err := idx.store.ListSymbolsByLanguage(ctx, rec.Language)
			if err != nil {
				idx.log.Warn("list symbols by language error", "lang", rec.Language, "err", err)
			} else {
				if runNameToIDByLang[rec.Language] == nil {
					runNameToIDByLang[rec.Language] = make(map[string]int64, len(syms))
				}
				for _, sym := range syms {
					runNameToIDByLang[rec.Language][sym.QualifiedName] = sym.ID
				}
			}
			loadedLangMaps[rec.Language] = true
		}

		reusableEmbeddings := map[string][]float32{}
		if prev, ok := existing[rec.RelPath]; ok {
			oldSymbols, err := idx.store.ListSymbolsByFile(ctx, prev.ID)
			if err != nil {
				idx.log.Warn("list old symbols error", "fileID", prev.ID, "err", err)
			}
			for _, old := range oldSymbols {
				if runNameToIDByLang[rec.Language] != nil {
					delete(runNameToIDByLang[rec.Language], old.QualifiedName)
				}
				if vec, ok, err := idx.store.GetEmbedding(ctx, old.ID); err == nil && ok {
					reusableEmbeddings[embeddingTextHash(rec.RelPath, rec.Language, old)] = vec
				} else if err != nil {
					idx.log.Warn("get old embedding error", "symbolID", old.ID, "err", err)
				}
			}
			if err := idx.store.DeleteSymbolsByFile(ctx, prev.ID); err != nil {
				idx.log.Warn("delete stale symbols error", "id", prev.ID, "err", err)
			}
		}

		fileID, err := idx.store.SaveFile(ctx, &store.File{
			Path:     rec.RelPath,
			Language: rec.Language,
			Hash:     rec.Hash,
			Mtime:    rec.ModTime,
			Size:     rec.Size,
		})
		if err != nil {
			idx.log.Warn("save file error", "path", rec.RelPath, "err", err)
			continue
		}

		symbols := extractor.Symbols(tree, source)
		for i := range symbols {
			symbols[i].FileID = fileID
			// Normalize before the overlay merges in: enrichment summaries are
			// already prose, and every extractor benefits from the same rule.
			symbols[i].Docstring = NormalizeDocstring(symbols[i].Docstring)
			if overlay, ok := idx.docOverlay[symbols[i].QualifiedName]; ok && overlay != "" {
				if symbols[i].Docstring == "" {
					symbols[i].Docstring = overlay
				} else {
					symbols[i].Docstring = overlay + "\n" + symbols[i].Docstring
				}
			}
		}

		var ids []int64
		if len(symbols) > 0 {
			ids, err = idx.store.SaveSymbolBatch(ctx, symbols)
			if err != nil {
				idx.log.Warn("save symbol batch error", "path", rec.RelPath, "err", err)
				continue
			}
		}

		for i, sym := range symbols {
			if runNameToIDByLang[rec.Language] == nil {
				runNameToIDByLang[rec.Language] = make(map[string]int64)
			}
			runNameToIDByLang[rec.Language][sym.QualifiedName] = ids[i]
		}
		edgeWork = append(edgeWork, edgeExtractionWork{tree: tree, source: source, language: rec.Language, extractor: extractor})

		if len(symbols) > 0 {
			var embedTexts []string
			var embedIndexes []int
			embeddings := make([]store.Embedding, 0, len(symbols))
			for i, sym := range symbols {
				text := EmbeddingText(rec.RelPath, rec.Language, sym)
				if vec, ok := reusableEmbeddings[hashString(text)]; ok {
					embeddings = append(embeddings, store.Embedding{SymbolID: ids[i], Vector: vec})
					continue
				}
				embedTexts = append(embedTexts, text)
				embedIndexes = append(embedIndexes, i)
			}
			// embedder == nil is the structure-only (--fast) mode: symbols,
			// edges and FTS are written now; embeddings are backfilled later
			// (BackfillEmbeddings finds the symbols with no vector row).
			if len(embedTexts) > 0 && idx.embedder != nil {
				vecs, err := idx.embedder.Embed(ctx, embedTexts)
				if err != nil {
					idx.log.Warn("embed error", "path", rec.RelPath, "err", err)
				} else {
					for i, vec := range vecs {
						symbolIndex := embedIndexes[i]
						embeddings = append(embeddings, store.Embedding{SymbolID: ids[symbolIndex], Vector: vec})
					}
				}
			}
			if len(embeddings) > 0 {
				if err := idx.store.UpsertEmbeddingBatch(ctx, embeddings); err != nil {
					idx.log.Warn("upsert embedding batch error", "path", rec.RelPath, "err", err)
				}
			}
		}

		stats.FilesIndexed++
		stats.Symbols += len(symbols)
		seen[rec.RelPath] = true
	}

	for range errs {
	}

	for _, work := range edgeWork {
		edges := work.extractor.Edges(work.tree, work.source, runNameToIDByLang[work.language])
		if len(edges) == 0 {
			continue
		}
		if err := idx.store.SaveEdgeBatch(ctx, edges); err != nil {
			idx.log.Warn("save edge batch error", "err", err)
			continue
		}
		stats.Edges += len(edges)
	}

	for path, f := range existing {
		if !seen[path] {
			if err := idx.store.DeleteFile(ctx, f.ID); err != nil {
				idx.log.Warn("delete removed file error", "path", path, "err", err)
			} else {
				stats.FilesDeleted++
			}
		}
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

func embeddingTextHash(path, language string, sym store.Symbol) string {
	return hashString(EmbeddingText(path, language, sym))
}

func hashString(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
