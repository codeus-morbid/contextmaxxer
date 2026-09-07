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

// DECISION(2026-08): a retrieval event reaches disk only once a label arrives
// for it. Writing every one produced 95958 retrieval events against 57 labels
// on this machine — 0.06% — because record_feedback is a call the agent makes
// voluntarily and mostly does not. That is 538MB of inputs with no outputs:
// enough to see what was served, never enough to learn what should have been,
// which is the only thing the log exists to support.
//
// So retrievals wait in a small ring keyed by request id, and RecordFeedback
// flushes the matching one just before the label. A request that never gets a
// label is never written; a label whose retrieval has aged out is still
// written, without its features, because the label itself is the scarce part.
// CONTEXTMAXXER_FEEDBACK_ALL=1 restores the old firehose for anyone debugging
// ranking, where seeing every served response is the point.
// ASSUMES: a label follows its retrieval within pendingRetrievals requests.
// REVISIT IF: labels start arriving from somewhere other than the same session.
const pendingRetrievals = 256

type Recorder struct {
	path     string
	maxBytes int64
	logAll   bool
	mu       sync.Mutex
	// pending holds retrieval events that have not been labelled yet, oldest
	// first in order; both are guarded by mu.
	pending map[string]RetrievalEvent
	order   []string
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
	return &Recorder{
		path:     path,
		maxBytes: max,
		logAll:   os.Getenv("CONTEXTMAXXER_FEEDBACK_ALL") == "1",
		pending:  make(map[string]RetrievalEvent, pendingRetrievals),
	}
}

// PendingPath is the single-entry breadcrumb holding the most recent
// unlabelled retrieval. The in-memory ring cannot serve the discovery hook,
// which runs as its own short-lived process and shares nothing with the
// server; without a file on disk a behavioural signal would arrive with a
// request id and no candidates, which is a label with nothing to label.
//
// It is one file, overwritten on every call and removed the moment the retrieval is
// labelled — a snapshot of the last query, not a history of every query.
func PendingPath(logPath string) string { return logPath + ".pending" }

// holdRetrieval parks an unlabelled retrieval, evicting the oldest once the
// ring is full. Eviction is silent on purpose: an unlabelled retrieval is
// exactly what this change stopped writing.
func (r *Recorder) holdRetrieval(event RetrievalEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.pending[event.RequestID]; !exists {
		r.order = append(r.order, event.RequestID)
	}
	r.pending[event.RequestID] = event
	for len(r.order) > pendingRetrievals {
		delete(r.pending, r.order[0])
		r.order = r.order[1:]
	}
	if data, err := json.Marshal(event); err == nil {
		_ = os.WriteFile(PendingPath(r.path), data, 0o644)
	}
}

// takeRetrieval removes and returns the held retrieval for a request id.
func (r *Recorder) takeRetrieval(requestID string) (RetrievalEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event, ok := r.pending[requestID]
	if !ok {
		return RetrievalEvent{}, false
	}
	delete(r.pending, requestID)
	for i, id := range r.order {
		if id == requestID {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return event, true
}

// OutcomeSearchedAfterContext marks a retrieval the agent followed with a text
// search of its own.
//
// DECISION(2026-08): the name describes what was OBSERVED, not what it means.
// It is tempting to call this a rejection — the agent had the answer and went
// looking anyway — but the same behaviour covers verifying a result, chasing a
// literal string, and a question the tool was never meant to answer (its own
// description tells agents to prefer grep on small or familiar repositories).
// Whoever trains on this decides what it is worth; the recorder's job is to
// stop losing the observation, not to grade it.
const OutcomeSearchedAfterContext = "searched_after_context"

// RecordObservedOutcome labels the last unlabelled retrieval from the outside,
// for callers that cannot see the server's memory — the discovery hook is a
// separate process, so the breadcrumb on disk is the only thing it can read.
//
// maxAge keeps an old breadcrumb from being blamed for something that happened
// much later. Returns false when there was nothing recent to label, which is
// the ordinary case and not an error.
func RecordObservedOutcome(logPath, outcome string, maxAge time.Duration) (bool, error) {
	data, err := os.ReadFile(PendingPath(logPath))
	if err != nil {
		return false, nil
	}
	var retrieval RetrievalEvent
	if err := json.Unmarshal(data, &retrieval); err != nil || retrieval.RequestID == "" {
		return false, nil
	}
	if maxAge > 0 && time.Since(retrieval.Time) > maxAge {
		return false, nil
	}
	// Consume it either way: a breadcrumb that has been acted on must not be
	// labelled twice by the next search in the same session.
	_ = os.Remove(PendingPath(logPath))

	r := NewRecorder(logPath)
	if r == nil {
		return false, nil
	}
	if err := r.append(retrieval); err != nil {
		return false, err
	}
	return true, r.append(FeedbackEvent{
		Event:     "feedback",
		RequestID: retrieval.RequestID,
		Time:      time.Now().UTC(),
		Outcome:   outcome,
		Source:    "hook",
	})
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
	if r.logAll {
		return r.append(event)
	}
	// Adoption telemetry goes to its own file, never here. This log is training
	// data — retrievals paired with the labels that judge them — and mixing a
	// per-call counter into it breaks that: the first attempt did, and two
	// existing tests caught it by asserting exactly what this log may contain.
	_ = recordServed(r.path, LooksPositional(event.Query))
	r.holdRetrieval(event)
	return nil
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
	// The retrieval goes first so a reader meets the candidates before the
	// label that judges them.
	if retrieval, ok := r.takeRetrieval(event.RequestID); ok {
		if err := r.append(retrieval); err != nil {
			return err
		}
	}
	_ = os.Remove(PendingPath(r.path))
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
