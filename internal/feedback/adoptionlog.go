package feedback

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The adoption log is telemetry, kept apart from the feedback log on purpose.
//
// feedback.jsonl is training data: a retrieval reaches it only once a label
// judges it, which is why counting "calls" from it would silently mean "calls
// that were followed by a search". Adoption needs the opposite — every call,
// with none of the payload. Putting both in one file cost two invariants on the
// first attempt, and the tests that guard the training log caught it.
//
// Nothing here stores a query, a candidate or a feature vector. A served line is
// about ninety bytes against the 5.6KB of a retrieval event, so the growth that
// forced a size cap on the other log does not apply.
const (
	adoptionSuffix = ".adoption.jsonl"
	// EventServed marks one answered call, and its shape.
	EventServed = "served"
	// EventHook marks something a hook observed.
	EventHook = "hook"
)

// AdoptionPath is where telemetry for a given feedback log lives.
func AdoptionPath(logPath string) string { return logPath + adoptionSuffix }

// LooksPositional classifies a query as a position rather than a sentence. It
// is installed by the composition root from the retrieval package's own rule, so
// the report counts by exactly what the pipeline routes on; the default keeps
// the field honest (rather than wrong) if nothing installed it.
var LooksPositional = func(string) bool { return false }

type adoptionEvent struct {
	Event      string    `json:"event"`
	Time       time.Time `json:"time"`
	Positional bool      `json:"positional,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	Note       string    `json:"note,omitempty"`
}

func recordServed(logPath string, positional bool) error {
	return appendAdoption(logPath, adoptionEvent{
		Event: EventServed, Time: time.Now().UTC(), Positional: positional,
	})
}

// RecordHookEvent appends one observation from a hook: the gate turning a search
// away, or the post-grep nudge being shown.
//
// Best-effort by construction, like everything a hook does: it runs on the
// agent's critical path, so the error is returned for tests and ignored by
// callers.
func RecordHookEvent(logPath, outcome, note string) error {
	return appendAdoption(logPath, adoptionEvent{
		Event: EventHook, Time: time.Now().UTC(), Outcome: outcome, Note: note,
	})
}

func appendAdoption(logPath string, e adoptionEvent) error {
	if logPath == "" {
		return nil
	}
	path := AdoptionPath(logPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// Outcomes recorded by the hooks rather than by the agent. Asking the agent does
// not work: record_feedback was called 57 times across 95958 served responses.
const (
	// OutcomeNudgeShown is written when the post-grep hook handed the agent a
	// concrete position to pass back. Note carries that position.
	OutcomeNudgeShown = "nudge_shown"
	// OutcomeGateBlocked is written when the discovery gate turned a search away.
	OutcomeGateBlocked = "gate_blocked"
)
