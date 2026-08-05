package index

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
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
