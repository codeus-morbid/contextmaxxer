package retrieve

import (
	"path/filepath"

	"github.com/codeus-morbid/contextmaxxer/internal/index"
)

// Tests in the index, out of the ranking until nothing else answers.
//
// Indexing name-conventioned tests is measured as a trade, not an upgrade: over
// 124 snapshots it moves file recall 0.572 -> 0.692 (t = 6.48) and precision
// 0.271 -> 0.221 (t = -4.09). Split by whether the answer lives in a test, the
// two halves point opposite ways — +0.171 on the 99 instances whose gold holds
// a test file, -0.077 on the 25 where it does not, because there test code
// displaces implementation.
//
// TestFloor was built to rescue that second group and does not: swept over the
// same 124 snapshots, the harmed instances sit at -0.077 whichever floor is set,
// unchanged to four decimals. Reordering a list cannot change which files are IN
// it, and file recall is membership — the harm is test symbols taking slots in
// the returned 40 and pushing implementation files off the end, not tests
// outranking implementation inside it.
//
// DECISION(2026-09): keep it anyway, for what it does do. A floor of 10 at
// max_results 40 recovers precision 0.221 -> 0.244 (t = 4.11, 38 instances
// better, 11 worse) for a file recall delta of exactly 0.0000 — no instance
// better, none worse. Under a line budget the answer's ORDER decides which
// results fit, so putting implementation first spends the budget on
// implementation. It is free, and on an index without tests it is a no-op.
// ASSUMES: max_results stays large enough that a floor leaves room for tests.
// REVISIT IF: a floor is wanted as a share of max_results rather than a count.
//
// A floor at or above max_results does change membership, by pulling non-tests
// up from deeper, and is measured as a bad trade here: the harmed group recovers
// to -0.044 while the 99 that benefit fall +0.171 -> +0.111. Not a default.
//
// It is a count of reserved slots rather than a score penalty on purpose.
// Cross-encoder scores are not guaranteed positive, and scaling a negative
// score by 0.5 moves the result UP — a penalty knob would have been backwards
// exactly where reranking was least certain.

// defaultTestFloor is the floor used when a request does not set one: a quarter
// of the answer, which is the ratio the sweep measured (10 of 40). Expressed as
// a share rather than the measured count because a count of 10 would swallow
// half a five-result answer, and the served max_results is not the probe's.
//
// A negative TestFloor switches the behaviour off; zero means "use this".
func defaultTestFloor(maxResults int) int {
	if maxResults <= 3 {
		return 0
	}
	return maxResults / 4
}

// walkerTestFile asks the indexer's own question of a stored path. The
// retrieval package's isTestFile is narrower (Go and TypeScript), and this
// sample is mostly Python, so using it here would have made the knob a no-op
// on the very instances the experiment is about.
func walkerTestFile(path string) bool {
	if path == "" {
		return false
	}
	return index.IsTestFile(filepath.Base(filepath.ToSlash(path)))
}

// applyTestFloor reserves the first `floor` answer slots for non-test files.
//
// Results keep their ranked order within each group, so this never invents a
// ranking of its own: it only decides who is allowed near the top. A floor at
// or above max_results means tests appear only once implementation candidates
// run out, which is the "in the index, out of the ranking" shape.
func applyTestFloor(scored []ScoredResult, floor int) []ScoredResult {
	if floor <= 0 || len(scored) == 0 {
		return scored
	}
	var head, rest []ScoredResult
	for _, s := range scored {
		if len(head) < floor && !walkerTestFile(s.File) {
			head = append(head, s)
			continue
		}
		rest = append(rest, s)
	}
	if len(rest) == 0 {
		return head
	}
	// Nothing is dropped: a test that lost a top slot lands behind the
	// non-tests that earned one, and the packer still cuts by budget.
	return append(head, rest...)
}
