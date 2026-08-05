package enrich

import "testing"

func TestStripEchoTail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{
			"Creates a session for a user, distinguishing it from similar symbols by encapsulating limits.",
			"Creates a session for a user.",
		},
		{
			"Rotates stored API keys; it sets it apart from RotateMasterKey by touching provider keys only.",
			"Rotates stored API keys.",
		},
		{
			"Parses CSV rows into employee records.",
			"Parses CSV rows into employee records.",
		},
		{
			"Unlike similar functions, this parses CSV rows.",
			"Unlike similar functions, this parses CSV rows.", // marker at start: keep as-is rather than emit empty
		},
		{
			"Computes the blend coefficient, compared to other rankers it trusts seeds.",
			"Computes the blend coefficient.",
		},
	}
	for _, c := range cases {
		if got := StripEchoTail(c.in); got != c.want {
			t.Errorf("StripEchoTail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCleanSummaryStripsWrappersAndEcho(t *testing.T) {
	in := "\"Summary: Saves a retrieval event to the feedback log, distinguishing itself from RecordFeedback.\"\nextra line"
	want := "Saves a retrieval event to the feedback log."
	if got := cleanSummary(in); got != want {
		t.Errorf("cleanSummary = %q, want %q", got, want)
	}
}
