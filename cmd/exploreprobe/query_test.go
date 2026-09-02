package main

import (
	"strings"
	"testing"
)

// The shape a real `pro` instance arrives in, trimmed. Worked out by hand
// before the code: the title is the sentence after the "## Title:" heading, and
// the identifiers are the two backticked names plus the file path.
const issueSample = "## Title:  \n\n" +
	"List operations do not support removing multiple distinct elements\n\n" +
	"#### Description:  \n\n" +
	"Currently, the `listRemove` method only handles one element at a time.\n" +
	"See src/database/list.js and the helper `listPush`.\n\n" +
	"### Steps to Reproduce:\n\n" +
	"1. Create a new list.\n"

func TestIssueTitle_SkipsTheHeadingItself(t *testing.T) {
	got := issueTitle(issueSample)
	want := "List operations do not support removing multiple distinct elements"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestIssueTitle_PlainFirstLine(t *testing.T) {
	// SWE-bench verified instances usually open with the title directly.
	text := "merge(combine_attrs='override') does not copy attrs\n<!-- template -->\nmore"
	if got := issueTitle(text); got != "merge(combine_attrs='override') does not copy attrs" {
		t.Fatalf("got %q", got)
	}
}

func TestCodeIdentifiers_BacktickedFirstThenPathsAndNames(t *testing.T) {
	ids := codeIdentifiers(issueSample, 24)
	joined := strings.Join(ids, " ")
	for _, want := range []string{"listRemove", "listPush", "src/database/list.js"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q missing from %q", want, joined)
		}
	}
	// The author's own emphasis leads: backticked names come before the rest.
	if ids[0] != "listRemove" {
		t.Fatalf("expected the backticked name first, got %q (%v)", ids[0], ids)
	}
}

func TestCodeIdentifiers_DropsProseThatLooksLikeAPath(t *testing.T) {
	ids := codeIdentifiers("This happens e.g. when the value is null. See github.com for details.", 24)
	for _, bad := range []string{"e.g", "github.com"} {
		for _, got := range ids {
			if strings.EqualFold(got, bad) {
				t.Fatalf("%q should not be an identifier: %v", bad, ids)
			}
		}
	}
}

func TestShapeQuery_Modes(t *testing.T) {
	raw := shapeQuery(issueSample, queryRaw)
	if raw != issueSample {
		t.Fatal("raw must pass the text through untouched")
	}
	if got := shapeQuery(issueSample, queryTitle); strings.Contains(got, "Steps to Reproduce") {
		t.Fatalf("title mode still carries the body: %q", got)
	}
	combined := shapeQuery(issueSample, queryTitleI)
	if !strings.HasPrefix(combined, "List operations") || !strings.Contains(combined, "listRemove") {
		t.Fatalf("title+ids should carry both: %q", combined)
	}
	// An unknown mode must not silently empty the query.
	if got := shapeQuery(issueSample, "nonsense"); got != issueSample {
		t.Fatal("unknown mode must fall back to raw")
	}
}
