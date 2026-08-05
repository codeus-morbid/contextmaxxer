package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

type FileRecord struct {
	Path     string
	RelPath  string
	Language string
	Hash     string
	ModTime  int64
	Size     int64
}

type WalkerConfig struct {
	IncludeTests bool
}

type Walker struct {
	log        *slog.Logger
	cfg        WalkerConfig
	knownFiles map[string]store.File
}

func NewWalker(log *slog.Logger) *Walker {
	return &Walker{log: log}
}

func NewWalkerWithConfig(log *slog.Logger, cfg WalkerConfig) *Walker {
	return &Walker{log: log, cfg: cfg}
}

func (w *Walker) SetKnownFiles(files map[string]store.File) {
	w.knownFiles = make(map[string]store.File, len(files))
	for path, file := range files {
		w.knownFiles[path] = file
	}
}

var extToLanguage = map[string]string{
	".go":  "go",
	".ts":  "typescript",
	".tsx": "tsx",
	".js":  "javascript",
	".jsx": "javascript",
	".mjs": "javascript",
	".cjs": "javascript",
	".py":  "python",
	// Tier-2 languages (symbols-only extraction).
	".java": "java",
	".rs":   "rust",
	".c":    "c",
	// .h is ambiguous C/C++; the cpp grammar parses C headers too.
	".h":     "cpp",
	".cc":    "cpp",
	".cpp":   "cpp",
	".cxx":   "cpp",
	".hpp":   "cpp",
	".hh":    "cpp",
	".cs":    "csharp",
	".rb":    "ruby",
	".php":   "php",
	".kt":    "kotlin",
	".kts":   "kotlin",
	".scala": "scala",
	".sc":    "scala",
}

// DECISION: skipped dirs are checked by name only (not full path) for simplicity and speed.
var skipDirs = map[string]bool{
	".git":           true,
	"node_modules":   true,
	"vendor":         true,
	"dist":           true,
	"build":          true,
	"target":         true,
	".contextmaxxer": true,
}

// SkipDir reports whether a directory (by base name) should be excluded from
// both indexing and watching: known build/vendor/VCS dirs and any dotfile dir.
func SkipDir(name string) bool {
	return skipDirs[name] || strings.HasPrefix(name, ".")
}

// DECISION: test file patterns are filtered at the file level, not directory level,
// because some projects keep legitimate non-test code in directories named "test/".
// Patterns cover Go (_test.go), TS/JS (.test.*, .spec.*) and Python (test_*.py, *_test.py).
func isTestFile(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, "_test.go") {
		return true
	}
	testSuffixes := []string{
		"_test.ts", "_test.tsx", "_test.js", "_test.jsx",
		".test.ts", ".test.tsx", ".test.js", ".test.jsx",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
		"_test.py",
		// Tier-2 languages. Rust has no filename convention (inline
		// #[cfg(test)] modules), so it is not filterable here.
		"_test.rb", "_spec.rb",
		"_test.c", "_test.cc", "_test.cpp",
	}
	// Java/C#/PHP use CamelCase FooTest.java — matched case-sensitively so
	// e.g. "latest.java" isn't swallowed by a lowercase "test.java" suffix.
	for _, suf := range []string{"Test.java", "Tests.java", "Test.cs", "Tests.cs", "Test.php"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	for _, suf := range testSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	if strings.HasPrefix(lower, "test_") && strings.HasSuffix(lower, ".py") {
		return true
	}
	return false
}

const maxFileSize = 1 << 20 // 1 MB

// isGeneratedFile reports whether a source file carries the conventional
// "generated code" marker (Go's `// Code generated ... DO NOT EDIT.`, also used
// by many TS/JS/Python generators). Only the file head is read. Matching both
// substrings on one line avoids false positives on hand-written files.
func isGeneratedFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 2048)
	n, _ := f.Read(buf)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if strings.Contains(line, "Code generated") && strings.Contains(line, "DO NOT EDIT") {
			return true
		}
	}
	return false
}

func (w *Walker) Walk(ctx context.Context, root string) (<-chan FileRecord, <-chan error) {
	records := make(chan FileRecord, 64)
	errs := make(chan error, 1)

	go func() {
		defer close(records)
		defer close(errs)

		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				w.log.Warn("walk entry error", "path", path, "err", err)
				return nil
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if d.IsDir() {
				if SkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}

			if !w.cfg.IncludeTests && isTestFile(d.Name()) {
				w.log.Debug("skipping test file", "path", path)
				return nil
			}

			ext := strings.ToLower(filepath.Ext(d.Name()))
			lang, ok := extToLanguage[ext]
			if !ok {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				w.log.Warn("stat error", "path", path, "err", err)
				return nil
			}
			if info.Size() > maxFileSize {
				w.log.Debug("skipping large file", "path", path, "size", info.Size())
				return nil
			}

			// Skip machine-generated files (protobuf, goyacc, mockgen, ...). They
			// are search noise and, in bulk (e.g. CockroachDB), their symbol count
			// blows up the index. Detected by the conventional generated-code
			// marker near the top of the file.
			if isGeneratedFile(path) {
				w.log.Debug("skipping generated file", "path", path)
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				rel = path
			}

			hash := ""
			if known, ok := w.knownFiles[rel]; ok && known.Mtime == info.ModTime().Unix() && known.Size == info.Size() && known.Hash != "" {
				hash = known.Hash
			} else {
				hash, err = hashFile(path)
				if err != nil {
					w.log.Warn("hash error", "path", path, "err", err)
					return nil
				}
			}

			rec := FileRecord{
				Path:     path,
				RelPath:  rel,
				Language: lang,
				Hash:     hash,
				ModTime:  info.ModTime().Unix(),
				Size:     info.Size(),
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case records <- rec:
			}

			return nil
		})

		if err != nil && err != context.Canceled {
			errs <- err
		}
	}()

	return records, errs
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
