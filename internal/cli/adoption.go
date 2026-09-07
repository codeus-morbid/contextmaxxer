package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
)

// `contextmaxxer feedback adoption` answers the one question the benchmarks
// cannot: does an agent actually reach for this tool, and does the post-grep
// nudge change what it does next.
//
// It is deliberately built out of observations rather than self-report.
// record_feedback asks an agent to grade a result after it already has what it
// wanted, and across 95958 served responses it was called 57 times. Everything
// counted here is something a hook or the server saw happen.
//
// DECISION(2026-09): a nudge counts as followed when a position-shaped query
// arrives within a time window, because nothing in the log ties the two to one
// session — the hook knows the host's session id and the MCP server does not.
// The window is the same device, and the same reasoning, as the five minutes
// searchAfterContextWindow already uses. It over-counts when two things happen
// close together for unrelated reasons, so the window is reported next to the
// number rather than hidden inside it.
// ASSUMES: an agent that acts on the nudge does so in its next few turns.
// REVISIT IF: the served side learns the host session id, which would make this
// exact instead of probabilistic.

const defaultAdoptionWindow = 10 * time.Minute

// defaultFeedbackLogPath resolves the log the same way the hooks do, so a
// report run from the repository root reads what they wrote.
func defaultFeedbackLogPath() string {
	if p := os.Getenv("CONTEXTMAXXER_FEEDBACK_LOG"); p != "" && p != "none" {
		return p
	}
	return filepath.Join(".contextmaxxer", "feedback.jsonl")
}

type adoptionStats struct {
	retrievals       int
	positional       int
	nudges           int
	nudgesFollow     int
	gateBlocks       int
	searchAfter      int
	legacyRetrievals int
	first, last      time.Time
	window           time.Duration
}

// logEvent is the union of the two shapes the log holds, read loosely on
// purpose: an unknown or future event kind must be skipped, not fatal, because
// this reads a file that older and newer builds also write.
type logEvent struct {
	Event      string    `json:"event"`
	Time       time.Time `json:"time"`
	Query      string    `json:"query"`
	Outcome    string    `json:"outcome"`
	Positional bool      `json:"positional"`
}

func runFeedbackAdoption(args []string) error {
	path := defaultFeedbackLogPath()
	window := defaultAdoptionWindow
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-window":
			if i+1 >= len(args) {
				return fmt.Errorf("-window needs a duration, e.g. -window 5m")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("-window: %w", err)
			}
			window = d
			i++
		default:
			path = args[i]
		}
	}

	// The rotated generation is read too: rotation drops the oldest events by
	// design, and a report that ignored it would silently shrink its own window
	// the moment the log filled up.
	var events []logEvent
	read := 0
	// Telemetry lives beside the feedback log, not inside it.
	for _, p := range []string{feedback.AdoptionPath(path)} {
		evs, err := readLogEvents(p)
		if err != nil {
			continue
		}
		read++
		events = append(events, evs...)
	}
	if read == 0 {
		return fmt.Errorf("no feedback log at %s (nothing has been recorded yet)", path)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })

	st := computeAdoption(events, window)
	printAdoption(os.Stdout, path, st)
	return nil
}

// computeAdoption is separated from IO so the counting rules are testable
// without a file on disk.
func computeAdoption(events []logEvent, window time.Duration) adoptionStats {
	st := adoptionStats{window: window}
	var pendingNudges []time.Time

	for _, e := range events {
		if !e.Time.IsZero() {
			if st.first.IsZero() || e.Time.Before(st.first) {
				st.first = e.Time
			}
			if e.Time.After(st.last) {
				st.last = e.Time
			}
		}
		switch {
		case e.Event == "retrieval":
			// Legacy shape. A retrieval reaches the log only once a hook
			// observes an outcome for it, so these cannot be counted as calls
			// without quietly changing what "calls" means — they are reported
			// on their own line instead.
			st.legacyRetrievals++
		case e.Event == "served":
			st.retrievals++
			if e.Positional {
				st.positional++
				// Credit every nudge still inside the window: the agent may
				// have been nudged twice before acting once, and dropping the
				// older one would flatter the conversion rate.
				kept := pendingNudges[:0]
				for _, n := range pendingNudges {
					if e.Time.Sub(n) <= window && !e.Time.Before(n) {
						st.nudgesFollow++
						continue
					}
					kept = append(kept, n)
				}
				pendingNudges = kept
			}
		case e.Outcome == feedback.OutcomeNudgeShown:
			st.nudges++
			pendingNudges = append(pendingNudges, e.Time)
		case e.Outcome == feedback.OutcomeGateBlocked:
			st.gateBlocks++
		case e.Outcome == feedback.OutcomeSearchedAfterContext:
			st.searchAfter++
		}
	}
	return st
}

func printAdoption(w io.Writer, path string, st adoptionStats) {
	pct := func(a, b int) string {
		if b == 0 {
			return "  n/a"
		}
		return fmt.Sprintf("%5.1f%%", 100*float64(a)/float64(b))
	}
	fmt.Fprintf(w, "Adoption, from %s\n", path)
	if !st.first.IsZero() {
		fmt.Fprintf(w, "  covering %s .. %s\n",
			st.first.Local().Format("2006-01-02 15:04"), st.last.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "\n  find_context calls            %6d\n", st.retrievals)
	fmt.Fprintf(w, "    asked as a position         %6d  %s\n", st.positional, pct(st.positional, st.retrievals))
	fmt.Fprintf(w, "    asked as a sentence         %6d  %s\n", st.retrievals-st.positional, pct(st.retrievals-st.positional, st.retrievals))
	fmt.Fprintf(w, "\n  post-grep nudges shown        %6d\n", st.nudges)
	fmt.Fprintf(w, "    followed by a position      %6d  %s   (within %s)\n",
		st.nudgesFollow, pct(st.nudgesFollow, st.nudges), st.window)
	fmt.Fprintf(w, "\n  gate blocked a search         %6d\n", st.gateBlocks)
	fmt.Fprintf(w, "  searched anyway after an answer %4d\n", st.searchAfter)

	switch {
	case st.retrievals == 0:
		fmt.Fprintf(w, "\nNothing served yet. These counters only fill up through normal use.\n")
	case st.nudges == 0:
		fmt.Fprintf(w, "\nNo nudge has fired yet, so the conversion line says nothing either way.\n")
	case st.nudges < 10:
		fmt.Fprintf(w, "\n%d nudges is too few to read as a rate; treat it as a smoke test that\nthe instrument records both halves.\n", st.nudges)
	}
}

func readLogEvents(path string) ([]logEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// A retrieval event carries up to thirty candidates with feature vectors —
	// about 5.6KB measured, and larger on a wide response.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []logEvent
	for sc.Scan() {
		var e logEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // a truncated tail line is not worth failing a report over
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
