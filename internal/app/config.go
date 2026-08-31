package app

import (
	"flag"
	"os"
)

type Config struct {
	IndexPath    string
	LogLevel     string
	ProjectRoot  string
	NoEmbeddings bool
	IncludeTests bool
	ModelName    string
	ForceReindex bool
	// IndexPathSet records whether -index was given on the command line, so a
	// subcommand that declares its own -index can take the global one as its
	// default without overriding a default with a default.
	IndexPathSet bool
}

// ConfigFromFlags returns a *Config so that flag.Parse() (called by the caller
// after this function returns) writes parsed values into the same struct the
// caller holds. Returning by value would give the caller a copy whose fields
// are never updated by flag.Parse, since flag.StringVar binds to the address
// of the local cfg here.
func ConfigFromFlags() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.IndexPath, "index", defaultEnv("CONTEXTMAXXER_INDEX", ".contextmaxxer/index.db"), "path to index db")
	flag.StringVar(&cfg.LogLevel, "log-level", defaultEnv("CONTEXTMAXXER_LOG_LEVEL", "info"), "log level")
	flag.StringVar(&cfg.ProjectRoot, "root", defaultEnv("CONTEXTMAXXER_ROOT", "."), "project root to index")
	flag.BoolVar(&cfg.NoEmbeddings, "no-embeddings", false, "structure-only index: symbols/graph/FTS now, embeddings backfilled later (plain index run or MCP server)")
	flag.BoolVar(&cfg.NoEmbeddings, "fast", false, "alias for -no-embeddings")
	flag.BoolVar(&cfg.IncludeTests, "include-tests", false, "include test files in the index")
	flag.StringVar(&cfg.ModelName, "model", defaultEnv("CONTEXTMAXXER_MODEL", "jina-embeddings-v2-base-code"), "embedding model name")
	flag.BoolVar(&cfg.ForceReindex, "force", false, "reindex all files even if unchanged (needed after enrich so summaries reach embeddings)")
	return cfg
}

func defaultEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
