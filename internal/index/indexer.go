package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

type contentVersionStore interface {
	SetIndexContentVersion(ctx context.Context, version int) error
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
	tree          *tree_sitter.Tree
	source        []byte
	language      string
	extractor     LanguageExtractor
	localNameToID map[string]int64
}

type qualifiedNameIDs struct {
	first int64
	more  []int64
}

func addQualifiedNameID(names map[string]qualifiedNameIDs, name string, id int64) {
	ids := names[name]
	if ids.first == 0 {
		ids.first = id
		names[name] = ids
		return
	}
	if ids.first == id {
		return
	}
	for _, existing := range ids.more {
		if existing == id {
			return
		}
	}
	ids.more = append(ids.more, id)
	names[name] = ids
}

func removeQualifiedNameID(names map[string]qualifiedNameIDs, name string, id int64) {
	ids, ok := names[name]
	if !ok {
		return
	}
	if ids.first == id {
		if len(ids.more) == 0 {
			delete(names, name)
			return
		}
		ids.first = ids.more[0]
		ids.more = ids.more[1:]
		names[name] = ids
		return
	}
	for i, existing := range ids.more {
		if existing == id {
			ids.more = append(ids.more[:i], ids.more[i+1:]...)
			names[name] = ids
			return
		}
	}
}

func unambiguousQualifiedNames(names map[string]qualifiedNameIDs) map[string]int64 {
	result := make(map[string]int64, len(names))
	for name, ids := range names {
		if ids.first != 0 && len(ids.more) == 0 {
			result[name] = ids.first
		}
	}
	return result
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
	runNamesByLang := make(map[string]map[string]qualifiedNameIDs)
	loadedLangMaps := make(map[string]bool)
	var edgeWork []edgeExtractionWork
	// Files that called into a file being replaced. Deleting a file's symbols
	// cascades to the edges pointing at them, and a caller whose own bytes did
	// not change is skipped by hash, so nothing would re-resolve its call.
	// These are re-parsed for edges once every new symbol id exists.
	edgeDependents := make(map[int64]bool)
	reindexedFiles := make(map[int64]bool)

	// DECISION(2026-08): every parsed tree is registered for closing here.
	// tree_sitter.Tree wraps a C AST that the binding frees only in Close and
	// guards with no finalizer, so a dropped tree is native memory held until
	// the process exits. Trees cannot be closed where they are parsed — the
	// edge pass below re-walks them after every file is known — and several
	// paths between parse and that pass `continue` out, so one list closed on
	// the way out is the only shape that covers all of them. Within a single
	// Index call this changes little; the leak that matters is `--watch`, where
	// Index runs again on every change and the last run's ASTs are never freed.
	var openTrees []*tree_sitter.Tree
	defer func() {
		for _, t := range openTrees {
			t.Close()
		}
	}()

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
		openTrees = append(openTrees, tree)

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
				if runNamesByLang[rec.Language] == nil {
					runNamesByLang[rec.Language] = make(map[string]qualifiedNameIDs, len(syms))
				}
				for _, sym := range syms {
					addQualifiedNameID(runNamesByLang[rec.Language], sym.QualifiedName, sym.ID)
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
				if runNamesByLang[rec.Language] != nil {
					removeQualifiedNameID(runNamesByLang[rec.Language], old.QualifiedName, old.ID)
				}
				if vec, ok, err := idx.store.GetEmbedding(ctx, old.ID); err == nil && ok {
					reusableEmbeddings[embeddingTextHash(rec.RelPath, rec.Language, old)] = vec
				} else if err != nil {
					idx.log.Warn("get old embedding error", "symbolID", old.ID, "err", err)
				}
			}
			idx.collectEdgeDependents(ctx, oldSymbols, prev.ID, edgeDependents)
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
		reindexedFiles[fileID] = true

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

		localNameToID := make(map[string]int64, len(symbols))
		for i, sym := range symbols {
			if runNamesByLang[rec.Language] == nil {
				runNamesByLang[rec.Language] = make(map[string]qualifiedNameIDs)
			}
			addQualifiedNameID(runNamesByLang[rec.Language], sym.QualifiedName, ids[i])
			localNameToID[sym.QualifiedName] = ids[i]
		}
		edgeWork = append(edgeWork, edgeExtractionWork{
			tree: tree, source: source, language: rec.Language, extractor: extractor, localNameToID: localNameToID,
		})

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
			if idx.embedder != nil && ChunkingEnabled() {
				idx.embedChunks(ctx, rec.RelPath, rec.Language, symbols, ids)
			}
		}

		stats.FilesIndexed++
		stats.Symbols += len(symbols)
		seen[rec.RelPath] = true
	}

	for range errs {
	}

	unambiguousByLang := make(map[string]map[string]int64, len(runNamesByLang))
	for language, names := range runNamesByLang {
		unambiguousByLang[language] = unambiguousQualifiedNames(names)
	}
	for _, work := range edgeWork {
		nameToID := unambiguousByLang[work.language]
		if nameToID == nil {
			nameToID = make(map[string]int64)
			unambiguousByLang[work.language] = nameToID
		}
		type previousID struct {
			id     int64
			exists bool
		}
		previous := make(map[string]previousID, len(work.localNameToID))
		// DECISION(2026-08): global qnames resolve only when unique; symbols from
		// the current file temporarily override that map so duplicate package names
		// (especially main.main) still produce edges from the correct source ID.
		// ASSUMES: unresolved cross-directory duplicate callees are safer than false
		// edges. REVISIT IF: Go qnames become import-path-qualified.
		for name, id := range work.localNameToID {
			old, exists := nameToID[name]
			previous[name] = previousID{id: old, exists: exists}
			nameToID[name] = id
		}
		edges := work.extractor.Edges(work.tree, work.source, nameToID)
		for name, old := range previous {
			if old.exists {
				nameToID[name] = old.id
			} else {
				delete(nameToID, name)
			}
		}
		if len(edges) == 0 {
			continue
		}
		if err := idx.store.SaveEdgeBatch(ctx, edges); err != nil {
			idx.log.Warn("save edge batch error", "err", err)
			continue
		}
		stats.Edges += len(edges)
	}

	stats.Edges += idx.repairDependentEdges(ctx, root, edgeDependents, reindexedFiles, existingList, &openTrees)

	for path, f := range existing {
		if !seen[path] {
			if err := idx.store.DeleteFile(ctx, f.ID); err != nil {
				idx.log.Warn("delete removed file error", "path", path, "err", err)
			} else {
				stats.FilesDeleted++
			}
		}
	}
	if idx.forceReindex || len(existingList) == 0 {
		if versioned, ok := idx.store.(contentVersionStore); ok {
			if err := versioned.SetIndexContentVersion(ctx, store.CurrentIndexContentVersion); err != nil {
				return stats, fmt.Errorf("mark lossless index content: %w", err)
			}
		}
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

// collectEdgeDependents records which OTHER files hold edges into the symbols
// that are about to be deleted. Their call sites are still in their source;
// only the ids those calls resolved to are going away.
func (idx *Indexer) collectEdgeDependents(ctx context.Context, oldSymbols []store.Symbol, fileID int64, out map[int64]bool) {
	if len(oldSymbols) == 0 {
		return
	}
	ids := make([]int64, 0, len(oldSymbols))
	for _, sym := range oldSymbols {
		ids = append(ids, sym.ID)
	}
	callers, err := idx.store.GetCallerEdges(ctx, ids, 0)
	if err != nil {
		idx.log.Warn("get caller edges error", "fileID", fileID, "err", err)
		return
	}
	srcIDs := make([]int64, 0, len(callers))
	for _, srcs := range callers {
		srcIDs = append(srcIDs, srcs...)
	}
	if len(srcIDs) == 0 {
		return
	}
	srcSymbols, err := idx.store.GetSymbolsByIDs(ctx, srcIDs)
	if err != nil {
		idx.log.Warn("get caller symbols error", "fileID", fileID, "err", err)
		return
	}
	for _, sym := range srcSymbols {
		if sym.FileID != fileID {
			out[sym.FileID] = true
		}
	}
}

// repairDependentEdges re-extracts edges out of files that were skipped by
// hash but lost edges when a callee they point at was replaced. Without it an
// incremental run silently shrinks the call graph: the caller's bytes are
// unchanged, so nothing re-reads it, and its edge stays deleted until someone
// reindexes with --force.
//
// Names resolve against the database rather than against this run's map: every
// symbol written by this run is already committed, so the database is the
// complete picture and does not depend on which files happened to change.
func (idx *Indexer) repairDependentEdges(
	ctx context.Context,
	root string,
	dependents, reindexed map[int64]bool,
	files []store.File,
	openTrees *[]*tree_sitter.Tree,
) int {
	if len(dependents) == 0 {
		return 0
	}
	byID := make(map[int64]store.File, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}
	namesByLang := make(map[string]map[string]int64)
	repaired := 0

	for fileID := range dependents {
		if reindexed[fileID] {
			continue
		}
		file, ok := byID[fileID]
		if !ok {
			continue
		}
		extractor, ok := idx.extractor(file.Language)
		if !ok {
			continue
		}
		source, err := os.ReadFile(filepath.Join(root, file.Path))
		if err != nil {
			idx.log.Warn("reread caller file error", "path", file.Path, "err", err)
			continue
		}
		tree, err := idx.parser.Parse(ctx, source, file.Language)
		if err != nil {
			idx.log.Warn("reparse caller file error", "path", file.Path, "err", err)
			continue
		}
		*openTrees = append(*openTrees, tree)

		nameToID, ok := namesByLang[file.Language]
		if !ok {
			nameToID = idx.unambiguousNamesFromStore(ctx, file.Language)
			namesByLang[file.Language] = nameToID
		}
		symbols, err := idx.store.ListSymbolsByFile(ctx, fileID)
		if err != nil {
			idx.log.Warn("list caller symbols error", "fileID", fileID, "err", err)
			continue
		}
		// Same local-override rule as the main pass: a duplicate qualified name
		// resolves to this file's symbol while extracting this file.
		restore := make(map[string]previousName, len(symbols))
		for _, sym := range symbols {
			old, exists := nameToID[sym.QualifiedName]
			restore[sym.QualifiedName] = previousName{id: old, exists: exists}
			nameToID[sym.QualifiedName] = sym.ID
		}
		edges := extractor.Edges(tree, source, nameToID)
		for name, old := range restore {
			if old.exists {
				nameToID[name] = old.id
			} else {
				delete(nameToID, name)
			}
		}
		if len(edges) == 0 {
			continue
		}
		if err := idx.store.SaveEdgeBatch(ctx, edges); err != nil {
			idx.log.Warn("save repaired edge batch error", "path", file.Path, "err", err)
			continue
		}
		repaired += len(edges)
	}
	return repaired
}

type previousName struct {
	id     int64
	exists bool
}

// unambiguousNamesFromStore maps every qualified name of a language to its
// symbol id, dropping the names more than one symbol claims — the same rule
// the main pass applies, so a repaired edge and a fresh one agree.
func (idx *Indexer) unambiguousNamesFromStore(ctx context.Context, language string) map[string]int64 {
	syms, err := idx.store.ListSymbolsByLanguage(ctx, language)
	if err != nil {
		idx.log.Warn("list symbols by language error", "lang", language, "err", err)
		return map[string]int64{}
	}
	names := make(map[string]qualifiedNameIDs, len(syms))
	for _, sym := range syms {
		addQualifiedNameID(names, sym.QualifiedName, sym.ID)
	}
	return unambiguousQualifiedNames(names)
}

func embeddingTextHash(path, language string, sym store.Symbol) string {
	return hashString(EmbeddingText(path, language, sym))
}

func hashString(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// embedChunks gives the capped symbols of one file a vector per window. It is
// best-effort in exactly the way the per-symbol embedding is: a failure costs
// the extra channel, never the file's index entry.
func (idx *Indexer) embedChunks(ctx context.Context, relPath, language string, symbols []store.Symbol, ids []int64) {
	var chunks []store.SymbolChunk
	for i, sym := range symbols {
		sym.ID = ids[i]
		chunks = append(chunks, ChunkSymbol(relPath, language, sym)...)
	}
	if len(chunks) == 0 {
		return
	}
	chunkIDs, err := idx.store.SaveSymbolChunks(ctx, chunks)
	if err != nil {
		idx.log.Warn("save chunks error", "path", relPath, "err", err)
		return
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vecs, err := idx.embedder.Embed(ctx, texts)
	if err != nil {
		idx.log.Warn("embed chunks error", "path", relPath, "err", err)
		return
	}
	if len(vecs) != len(chunkIDs) {
		idx.log.Warn("embed chunks count mismatch", "path", relPath, "want", len(chunkIDs), "got", len(vecs))
		return
	}
	embeddings := make([]store.ChunkEmbedding, len(vecs))
	for i, vec := range vecs {
		embeddings[i] = store.ChunkEmbedding{ChunkID: chunkIDs[i], Vector: vec}
	}
	if err := idx.store.UpsertChunkEmbeddingBatch(ctx, embeddings); err != nil {
		idx.log.Warn("upsert chunk embeddings error", "path", relPath, "err", err)
	}
}
