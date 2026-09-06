package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
)

func TestComputeAdoption(t *testing.T) {
	t0 := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	ev := func(d time.Duration, event, query, outcome string) logEvent {
		return logEvent{Event: event, Time: t0.Add(d), Query: query, Outcome: outcome}
	}
	events := []logEvent{
		ev(0, "hook", "", feedback.OutcomeNudgeShown),
		// Acted on two minutes later: this is the conversion being measured.
		ev(2*time.Minute, "retrieval", "src/topics/posts.js:142", ""),
		ev(time.Hour, "hook", "", feedback.OutcomeNudgeShown),
		// A sentence is not a conversion, however soon it arrives.
		ev(time.Hour+time.Minute, "retrieval", "how are recent topics sorted", ""),
		// A position half an hour later is outside the window: the nudge that
		// preceded it must not be credited.
		ev(time.Hour+30*time.Minute, "retrieval", "src/other.js:9", ""),
		ev(2*time.Hour, "hook", "", feedback.OutcomeGateBlocked),
		ev(2*time.Hour, "feedback", "", feedback.OutcomeSearchedAfterContext),
	}

	st := computeAdoption(events, 10*time.Minute)

	if st.retrievals != 3 {
		t.Fatalf("retrievals = %d, want 3", st.retrievals)
	}
	if st.positional != 2 {
		t.Fatalf("positional = %d, want 2", st.positional)
	}
	if st.nudges != 2 {
		t.Fatalf("nudges = %d, want 2", st.nudges)
	}
	if st.nudgesFollow != 1 {
		t.Fatalf("nudges followed = %d, want 1 (the second position is outside the window)", st.nudgesFollow)
	}
	if st.gateBlocks != 1 || st.searchAfter != 1 {
		t.Fatalf("gateBlocks=%d searchAfter=%d, want 1 and 1", st.gateBlocks, st.searchAfter)
	}
}

func TestComputeAdoption_OneActionCreditsEveryOpenNudge(t *testing.T) {
	// An agent nudged twice before it acts once has been nudged twice; crediting
	// only the latest would flatter the rate.
	t0 := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	events := []logEvent{
		{Event: "hook", Time: t0, Outcome: feedback.OutcomeNudgeShown},
		{Event: "hook", Time: t0.Add(time.Minute), Outcome: feedback.OutcomeNudgeShown},
		{Event: "retrieval", Time: t0.Add(2 * time.Minute), Query: "internal/retrieve/locator.go:73"},
	}
	st := computeAdoption(events, 10*time.Minute)
	if st.nudgesFollow != 2 {
		t.Fatalf("nudges followed = %d, want 2", st.nudgesFollow)
	}
}

func TestPrintAdoptionWarnsWhenTheSampleIsTooSmallToRead(t *testing.T) {
	var buf bytes.Buffer
	printAdoption(&buf, "log.jsonl", adoptionStats{retrievals: 5, nudges: 2, nudgesFollow: 1, window: time.Minute})
	out := buf.String()
	if !strings.Contains(out, "too few to read as a rate") {
		t.Fatalf("a two-nudge sample must not be presented as a rate:\n%s", out)
	}

	buf.Reset()
	printAdoption(&buf, "log.jsonl", adoptionStats{retrievals: 0, window: time.Minute})
	if !strings.Contains(buf.String(), "Nothing served yet") {
		t.Fatalf("an empty log must say so:\n%s", buf.String())
	}
}
