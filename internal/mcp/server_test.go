package mcp

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
	"github.com/codeus-morbid/contextmaxxer/internal/retrieve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServer_Constructs(t *testing.T) {
	// DECISION: Retriever has no interface used by Server — test that construction
	// succeeds with a nil retriever (Serve is not called in this test).
	var r *retrieve.Retriever
	srv := NewServer(r, slog.Default())
	assert.NotNil(t, srv)
}

func TestServerRecordsFeedback(t *testing.T) {
	rec := feedback.NewRecorder(filepath.Join(t.TempDir(), "feedback.jsonl"))
	srv := NewServerWithFeedback(nil, rec, slog.Default())

	require.NoError(t, srv.recordFeedback(context.Background(), feedback.FeedbackEvent{
		RequestID:       "req-1",
		Query:           "find context",
		SelectedSymbols: []string{"pkg.Target"},
		Outcome:         "used",
		Source:          "test",
	}))
}
