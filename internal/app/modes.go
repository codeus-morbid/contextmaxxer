package app

import (
	"context"
	"errors"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/cli"
)

func runIndex(ctx context.Context, a *App) error {
	return cli.RunIndex(ctx, a.indexArgs, a.logger, a.cfg.IndexPath, a.cfg.NoEmbeddings, a.cfg.IncludeTests, a.cfg.ModelName, a.cfg.ForceReindex)
}

func runInit(ctx context.Context, a *App) error {
	return cli.RunInit(ctx, a.indexArgs, a.logger, a.cfg.NoEmbeddings, a.cfg.IncludeTests, a.cfg.ModelName)
}

func runEnrich(ctx context.Context, a *App) error {
	return cli.RunEnrich(ctx, a.indexArgs, a.logger, a.cfg.ModelName)
}

func runFeedback(ctx context.Context, a *App) error {
	return cli.RunFeedback(ctx, a.indexArgs, a.logger)
}

func runWarmup(ctx context.Context, a *App) error {
	return cli.RunWarmup(ctx, a.indexArgs, a.logger, a.cfg.ModelName)
}

// withGlobalIndex forwards an explicitly-set global -index to the subcommands
// that own a flag of the same name.
//
// DECISION(2026-08): `contextmaxxer --index X query "..."` used to run against
// the DEFAULT index, silently. query and mcp declare their own -index, the
// global value was never passed down, and the two flags spell identically — so
// the command looked right, exited 0 and answered from a different repository.
// It cost most of a debugging session: every result came from the wrong index
// while eight hypotheses were built on top of them. An explicit subcommand flag
// still wins; this only supplies the global one as its default.
func withGlobalIndex(a *App, args []string) []string {
	if !a.cfg.IndexPathSet || hasIndexFlag(args) {
		return args
	}
	return append([]string{"-index", a.cfg.IndexPath}, args...)
}

func hasIndexFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-index" || arg == "--index" ||
			strings.HasPrefix(arg, "-index=") || strings.HasPrefix(arg, "--index=") {
			return true
		}
	}
	return false
}

func runQuery(ctx context.Context, a *App) error {
	return cli.RunQuery(ctx, withGlobalIndex(a, a.indexArgs), a.logger)
}

func runMCP(ctx context.Context, a *App) error {
	return cli.RunMCP(ctx, withGlobalIndex(a, a.indexArgs), a.logger)
}

func runWatch(_ context.Context, _ *App) error {
	return errors.New("not implemented")
}
