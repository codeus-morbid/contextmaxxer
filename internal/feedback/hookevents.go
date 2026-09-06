package feedback

import "time"

// Outcomes recorded by the hooks rather than by the agent.
//
// The adoption question — does an agent actually reach for this tool, and does
// the post-grep nudge change what it does — cannot be answered by asking the
// agent. record_feedback was called 57 times across 95958 served responses. So
// the hooks write what they observed instead, and cmd `feedback adoption` reads
// both halves out of the same log.
const (
	// OutcomeNudgeShown is written when the post-grep hook handed the agent a
	// concrete position to pass back. Note carries that position.
	OutcomeNudgeShown = "nudge_shown"
	// OutcomeGateBlocked is written when the discovery gate blocked a search.
	OutcomeGateBlocked = "gate_blocked"
)

// RecordHookEvent appends one observation from a hook.
//
// Best-effort by construction, like everything else a hook does: it runs on the
// agent's critical path and a logging failure must never delay or block a tool
// call, so the error is returned for tests and ignored by callers.
func RecordHookEvent(logPath, outcome, note string) error {
	r := NewRecorder(logPath)
	if r == nil {
		return nil
	}
	return r.append(FeedbackEvent{
		Event:   "hook",
		Time:    time.Now().UTC(),
		Outcome: outcome,
		Note:    note,
		Source:  "hook",
	})
}
