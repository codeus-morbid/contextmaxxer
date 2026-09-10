package index

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestWalker() *Walker {
	return NewWalker(slog.Default())
}

func collectRecords(t *testing.T, ctx context.Context, records <-chan FileRecord, errs <-chan error) []FileRecord {
	t.Helper()
	var out []FileRecord
	for rec := range records {
		out = append(out, rec)
	}
	if err, ok := <-errs; ok && err != nil {
		t.Fatalf("walk error: %v", err)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
}

func TestWalker_FindsFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main")
	writeFile(t, filepath.Join(dir, "script.py"), "print('hello')")
	writeFile(t, filepath.Join(dir, "README.txt"), "ignore me")

	w := newTestWalker()
	recs, errs := w.Walk(context.Background(), dir)
	out := collectRecords(t, context.Background(), recs, errs)

	require.Len(t, out, 2)
	langs := map[string]bool{}
	for _, r := range out {
		langs[r.Language] = true
		require.NotEmpty(t, r.Hash)
		require.NotEmpty(t, r.RelPath)
	}
	require.True(t, langs["go"])
	require.True(t, langs["python"])
}

func TestWalker_SkipsHiddenAndIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "foo.go"), "package git")
	writeFile(t, filepath.Join(dir, "node_modules", "bar.go"), "package nm")
	writeFile(t, filepath.Join(dir, "src", "baz.go"), "package src")

	w := newTestWalker()
	recs, errs := w.Walk(context.Background(), dir)
	out := collectRecords(t, context.Background(), recs, errs)

	require.Len(t, out, 1)
	require.Equal(t, "baz.go", filepath.Base(out[0].Path))
}

func TestWalker_HashStable(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.go"), "package app")

	w := newTestWalker()

	recs1, errs1 := w.Walk(context.Background(), dir)
	out1 := collectRecords(t, context.Background(), recs1, errs1)
	require.Len(t, out1, 1)

	recs2, errs2 := w.Walk(context.Background(), dir)
	out2 := collectRecords(t, context.Background(), recs2, errs2)
	require.Len(t, out2, 1)

	require.Equal(t, out1[0].Hash, out2[0].Hash)
}

func TestWalker_UsesKnownFileHashWhenMetadataMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.go")
	writeFile(t, path, "package app")
	info, err := os.Stat(path)
	require.NoError(t, err)

	w := newTestWalker()
	w.SetKnownFiles(map[string]store.File{
		"app.go": {Path: "app.go", Language: "go", Hash: "stored-hash", Mtime: info.ModTime().Unix(), Size: info.Size()},
	})

	recs, errs := w.Walk(context.Background(), dir)
	out := collectRecords(t, context.Background(), recs, errs)

	require.Len(t, out, 1)
	require.Equal(t, "stored-hash", out[0].Hash)
}

func TestWalker_HashesFileWhenKnownMetadataDiffers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.go")
	writeFile(t, path, "package app")

	w := newTestWalker()
	w.SetKnownFiles(map[string]store.File{
		"app.go": {Path: "app.go", Language: "go", Hash: "stale-hash", Mtime: 1, Size: 1},
	})

	recs, errs := w.Walk(context.Background(), dir)
	out := collectRecords(t, context.Background(), recs, errs)

	require.Len(t, out, 1)
	require.NotEqual(t, "stale-hash", out[0].Hash)
	require.NotEmpty(t, out[0].Hash)
}

func TestWalker_RespectsContext(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		writeFile(t, filepath.Join(dir, filepath.Join("sub", fmt.Sprintf("f%d.go", i))), "package x")
	}

	ctx, cancel := context.WithCancel(context.Background())

	w := newTestWalker()
	recs, _ := w.Walk(ctx, dir)

	cancel()

	done := make(chan struct{})
	go func() {
		for range recs {
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("channel did not close after context cancel")
	}
}

// The gap this closes: a suite laid out by directory names its files models.py,
// urls.py and tests.py, so no basename rule can see them. Across 63 indexed
// projects that let 11% of all indexed files through as source — 85% of the
// pylint index, 57% of django's.
func TestIsTestSuiteDir(t *testing.T) {
	for _, p := range []string{
		"tests", "tests/admin_views", "test", "test/api",
		"src/pkg/tests", "a/b/tests/c",
	} {
		assert.True(t, IsTestSuiteDir(p), "%s holds a suite", p)
	}

	// The case the original file-level-only decision existed to protect:
	// django/test/ is the framework users import as django.test, not tests.
	// Singular and nested stays.
	for _, p := range []string{
		"", ".", "django/test", "django/test/client", "src/testing", "pkg/latest",
		"contrib/testdata", "internal/attest",
	} {
		assert.False(t, IsTestSuiteDir(p), "%s is not a suite", p)
	}
}

// Suites are indexed on purpose, and this pins the reason. Excluding
// directories was implemented and reverted: 308 of SWE-Explore's 3,659 gold
// files sit under one without a test-shaped name, across 224 of 848 instances,
// and gold there is what a solver had to read. Crowding is handled by the test
// floor in ranking, where demoting costs nothing that dropping would.
func TestWalkerKeepsSuiteDirectoriesForReading(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	write("django/db/base.py", "def f(): pass")
	write("django/test/client.py", "class Client: pass")
	write("tests/admin/tests.py", "def test_x(): pass")
	write("tests/admin/test_forms.py", "def test_y(): pass") // test-shaped name: still dropped

	w := NewWalkerWithConfig(slog.New(slog.NewTextHandler(io.Discard, nil)), WalkerConfig{})
	records, errs := w.Walk(context.Background(), root)
	var got []string
	for r := range records {
		got = append(got, filepath.ToSlash(r.RelPath))
	}
	require.NoError(t, <-errs)

	assert.ElementsMatch(t, []string{
		"django/db/base.py", "django/test/client.py", "tests/admin/tests.py",
	}, got, "the filename rule still drops test_forms.py; the directory is kept")
}
