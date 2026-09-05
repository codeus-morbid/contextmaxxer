package cli

import (
	"os"
	"strings"
	"testing"
)

// runPreSearch drives the gate the way a host does: JSON on stdin, exit code
// out. stderr is captured so the block message can be asserted.
func runPreSearch(t *testing.T, sessionID string) (code int, stderr string) {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "in-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString(`{"session_id":"` + sessionID + `"}`); err != nil {
		t.Fatal(err)
	}
	in.Close()

	stdin, stderrFile := os.Stdin, os.Stderr
	defer func() { os.Stdin, os.Stderr = stdin, stderrFile }()

	f, err := os.Open(in.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	errOut, err := os.CreateTemp(t.TempDir(), "err-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stderr = f, errOut
	code = RunHook([]string{"pre-search"})
	errOut.Close()
	data, _ := os.ReadFile(errOut.Name())
	return code, string(data)
}

func TestGateBlocksOnceAndThenGetsOutOfTheWay(t *testing.T) {
	// The deadlock this guards, seen live: the marker that opens the gate is
	// written by a PostToolUse hook, and that hook does not run when the tool
	// call FAILS. Against a corrupt index find_context errors, no marker is
	// written, and a gate that blocked every time would leave the agent with
	// neither search tool for the rest of the session.
	session := "gate-test-" + t.Name()
	marker := hookMarkerPath(session)
	t.Cleanup(func() {
		os.Remove(marker)
		os.Remove(marker + ".blocked")
		os.Remove(marker + ".nudged")
	})
	os.Remove(marker)
	os.Remove(marker + ".blocked")

	code, stderr := runPreSearch(t, session)
	if code != 2 {
		t.Fatalf("first search must be blocked, got exit %d", code)
	}
	if !strings.Contains(stderr, "Use find_context first") {
		t.Fatalf("block message missing: %q", stderr)
	}

	// find_context never succeeded, so the gate marker still does not exist.
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("test setup wrong: the gate marker should not exist")
	}
	code, _ = runPreSearch(t, session)
	if code != 0 {
		t.Fatalf("second search must pass even though find_context never answered, got exit %d", code)
	}
}

func TestGateStaysOpenOnceFindContextAnswered(t *testing.T) {
	session := "gate-open-" + t.Name()
	marker := hookMarkerPath(session)
	t.Cleanup(func() {
		os.Remove(marker)
		os.Remove(marker + ".blocked")
	})
	if err := os.WriteFile(marker, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _ := runPreSearch(t, session); code != 0 {
		t.Fatalf("gate must be open after find_context answered, got exit %d", code)
	}
}

func TestGateFailsOpenWithoutASessionID(t *testing.T) {
	if code, _ := runPreSearch(t, ""); code != 0 {
		t.Fatalf("no session id must never block, got exit %d", code)
	}
}
