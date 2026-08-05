package observ

import (
	"log/slog"
	"os"
)

func NewLogger(mode string) *slog.Logger {
	if mode == "mcp" {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}
