package app

import (
	"context"
	"errors"

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

func runQuery(ctx context.Context, a *App) error {
	return cli.RunQuery(ctx, a.indexArgs, a.logger)
}

func runMCP(ctx context.Context, a *App) error {
	return cli.RunMCP(ctx, a.indexArgs, a.logger)
}

func runWatch(_ context.Context, _ *App) error {
	return errors.New("not implemented")
}
