package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/goldensuite"
)

func TestDefaultConfigIsPublishedAndLoadable(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	configPath := filepath.Join(repoRoot, filepath.FromSlash(defaultConfigPath))

	cfg, base, err := goldensuite.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if cfg.GenManifest != "manifest.public.json" {
		t.Fatalf("gen manifest = %q, want manifest.public.json", cfg.GenManifest)
	}
	if _, err := os.Stat(filepath.Join(base, cfg.GenManifest)); err != nil {
		t.Fatalf("public manifest referenced by default config: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Name != "contextmaxxer" {
		t.Fatalf("repos = %#v, want only contextmaxxer", cfg.Repos)
	}

	wantIndex := filepath.Clean(filepath.Join(repoRoot, ".contextmaxxer", "index.db"))
	gotIndex := filepath.Clean(filepath.Join(base, cfg.Repos[0].Index))
	if gotIndex != wantIndex {
		t.Fatalf("contextmaxxer index = %q, want %q", gotIndex, wantIndex)
	}
}
