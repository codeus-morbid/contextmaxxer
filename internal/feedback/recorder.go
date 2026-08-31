package feedback

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// DECISION(2026-08): the log rotates at a size cap instead of growing forever.
// Every find_context call appends a retrieval event carrying up to thirty
// candidates with their full feature vectors — about 5.6KB each, measured — and
// nothing ever removed one. Two and a half months of dogfooding produced a
// 538MB file, seventy times the size of the index it describes, holding every
// query string and symbol path since the beginning. Disk is the smaller half of
// that: it is also a growing record of what someone searched for.
//
// One previous generation is kept, so the cap bounds the pair at 2x. Rotation
// loses the oldest events by design; feedback is training signal, not an audit
// trail, and the alternative on a shared machine is a file nobody notices until
// it is gigabytes.
// ASSUMES: recent events are the useful ones. REVISIT IF: a training run needs
// more history than one generation holds — raise the cap rather than removing it.
const (
	defaultMaxLogBytes = 64 << 20
	rotatedSuffix      = ".1"
)

type Recorder struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
}

func NewRecorder(path string) *Recorder {
	if path == "" {
		return nil
	}
	max := int64(defaultMaxLogBytes)
	if v := os.Getenv("CONTEXTMAXXER_FEEDBACK_MAX_MB"); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			max = mb << 20
		}
	}
	return &Recorder{path: path, maxBytes: max}
}

// RotatedPath is where the previous generation lives. Readers that want the
// whole retained history must read it before the current file.
func RotatedPath(path string) string { return path + rotatedSuffix }

// rotateIfLarge must be called with the lock held.
func (r *Recorder) rotateIfLarge() {
	info, err := os.Stat(r.path)
	if err != nil || info.Size() < r.maxBytes {
		return
	}
	// A failed rotation must not stop recording: the log is a convenience, and
	// refusing to serve a query because its log could not be renamed would be a
	// worse trade than a file that overshoots the cap.
	_ = os.Remove(RotatedPath(r.path))
	_ = os.Rename(r.path, RotatedPath(r.path))
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
	r.rotateIfLarge()
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
