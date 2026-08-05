package feedback

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Candidate struct {
	Rank          int                `json:"rank"`
	File          string             `json:"file"`
	QualifiedName string             `json:"qualified_name"`
	Kind          string             `json:"kind"`
	Score         float32            `json:"score"`
	Why           string             `json:"why"`
	Features      map[string]float32 `json:"features,omitempty"`
}

type RetrievalEvent struct {
	Event       string      `json:"event"`
	RequestID   string      `json:"request_id"`
	Time        time.Time   `json:"time"`
	Query       string      `json:"query"`
	Candidates  []Candidate `json:"candidates"`
	TotalTokens int         `json:"total_tokens,omitempty"`
	Source      string      `json:"source,omitempty"`
}

type FeedbackEvent struct {
	Event           string    `json:"event"`
	RequestID       string    `json:"request_id"`
	Time            time.Time `json:"time"`
	Query           string    `json:"query,omitempty"`
	SelectedSymbols []string  `json:"selected_symbols,omitempty"`
	RejectedSymbols []string  `json:"rejected_symbols,omitempty"`
	Outcome         string    `json:"outcome,omitempty"`
	Note            string    `json:"note,omitempty"`
	Source          string    `json:"source,omitempty"`
}

type Recorder struct {
	path string
	mu   sync.Mutex
}

func NewRecorder(path string) *Recorder {
	if path == "" {
		return nil
	}
	return &Recorder{path: path}
}

func NewRequestID() string {
	return uuid.NewString()
}

func (r *Recorder) RecordRetrieval(event RetrievalEvent) error {
	if r == nil {
		return nil
	}
	event.Event = "retrieval"
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if event.RequestID == "" {
		event.RequestID = NewRequestID()
	}
	return r.append(event)
}

func (r *Recorder) RecordFeedback(event FeedbackEvent) error {
	if r == nil {
		return nil
	}
	event.Event = "feedback"
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if event.RequestID == "" {
		return fmt.Errorf("feedback request_id is required")
	}
	return r.append(event)
}

func (r *Recorder) append(event any) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(r.path), 0755); err != nil {
		return fmt.Errorf("create feedback dir: %w", err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open feedback log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal feedback event: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write feedback event: %w", err)
	}
	return nil
}
