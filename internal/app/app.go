package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/codeus-morbid/contextmaxxer/internal/observ"
)

type App struct {
	cfg       Config
	logger    *slog.Logger
	indexArgs []string
}

func New(cfg Config) (*App, error) {
	logger := observ.NewLogger("cli")
	return &App{
		cfg:    cfg,
		logger: logger,
	}, nil
}

func (a *App) SetSubArgs(args []string) {
	a.indexArgs = args
}

func (a *App) Run(ctx context.Context, mode string) error {
	switch mode {
	case "init":
		return runInit(ctx, a)
	case "index":
		return runIndex(ctx, a)
	case "enrich":
		return runEnrich(ctx, a)
	case "feedback":
		return runFeedback(ctx, a)
	case "warmup":
		return runWarmup(ctx, a)
	case "query":
		return runQuery(ctx, a)
	case "mcp":
		return runMCP(ctx, a)
	case "watch":
		return runWatch(ctx, a)
	default:
		return fmt.Errorf("unknown mode: %s", mode)
	}
}
