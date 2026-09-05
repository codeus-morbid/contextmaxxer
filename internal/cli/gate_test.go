package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexedCwd is a project directory that looks indexed, so a test exercises
// the gate rather than the "nothing to send you to" escape.
func indexedCwd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".contextmaxxer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".contextmaxxer", "index.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runPreSearch drives the gate the way a host does: JSON on stdin, exit code
// out. stderr is captured so the block message can be asserted.
func runPreSearch(t *testing.T, sessionID string) (code int, stderr string) {
	return runPreSearchIn(t, sessionID, indexedCwd(t))
}

func runPreSearchIn(t *testing.T, sessionID, cwd string) (code int, stderr string) {
	t.Helper()
	in, err := os.CreateTemp(t.TempDir(), "in-*.json")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]string{"session_id": sessionID, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Write(payload); err != nil {
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

func TestGateStaysQuietInAnUnindexedProject(t *testing.T) {
	// Blocking here would spend the agent's turn to tell it to call a tool that
	// has no index to answer from.
	session := "gate-unindexed-" + t.Name()
	t.Cleanup(func() {
		os.Remove(hookMarkerPath(session))
		os.Remove(hookMarkerPath(session) + ".blocked")
	})
	code, stderr := runPreSearchIn(t, session, t.TempDir())
	if code != 0 {
		t.Fatalf("an unindexed project must not be gated, got exit %d", code)
	}
	if stderr != "" {
		t.Fatalf("nothing should be said either: %q", stderr)
	}
	// And no "blocked" marker was spent, so the one warning is still available
	// once the project is indexed.
	if _, err := os.Stat(hookMarkerPath(session) + ".blocked"); err == nil {
		t.Fatal("the single warning was consumed by an unindexed project")
	}
}

func TestIndexPresent(t *testing.T) {
	if indexPresent(indexedCwd(t)) != true {
		t.Fatal("an index.db with content should count as indexed")
	}
	if indexPresent(t.TempDir()) != false {
		t.Fatal("a directory with no .contextmaxxer should not count as indexed")
	}

	// An empty file is not an index; treating it as one would gate a repo whose
	// first index run was interrupted.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".contextmaxxer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".contextmaxxer", "index.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if indexPresent(dir) != false {
		t.Fatal("an empty index.db should not count as indexed")
	}
}
