package index

import "testing"

// Every extension the indexer parses must also arm the watcher. These were two
// separate lists and drifted: .mjs, .cjs, .cs, .php, .kt, .kts, .sc, .scala,
// .cxx and .hh were indexed but never triggered a reindex, so editing only such
// a file left the served index stale while the agent kept trusting it.
func TestWatchCoversEveryIndexedExtension(t *testing.T) {
	for ext := range extToLanguage {
		if !watchesExt(ext) {
			t.Errorf("extension %s is indexed but does not trigger a reindex", ext)
		}
	}
}

func TestWatchIgnoresNonSourceFiles(t *testing.T) {
	for _, ext := range []string{".log", ".tmp", ".swp", ".md", ""} {
		if watchesExt(ext) {
			t.Errorf("extension %q should not trigger a reindex", ext)
		}
	}
}
