package retrieve

import (
	"context"
	"path"
	"regexp"
	"sort"
	"strings"
)

// RelatedFile is one place that shares the vocabulary of an edit already made.
type RelatedFile struct {
	File    string
	Symbols []string
	Shared  []string // the rare names that put it here, best evidence first
	Score   float64
}

var relatedToken = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]+`)

// relatedStopWords carry no locating power. Without them the top of every
// answer is "self" and "return": they occur in most files, so they rank
// whatever is largest rather than whatever is related.
var relatedStopWords = map[string]bool{
	"self": true, "return": true, "if": true, "else": true, "for": true, "in": true,
	"not": true, "and": true, "or": true, "def": true, "class": true, "import": true,
	"from": true, "as": true, "is": true, "None": true, "True": true, "False": true,
	"try": true, "except": true, "raise": true, "with": true, "while": true,
	"the": true, "this": true, "that": true, "get": true, "set": true, "value": true,
	"args": true, "kwargs": true, "func": true, "var": true, "const": true,
	"err": true, "nil": true, "string": true, "int": true, "bool": true, "error": true,
}

// relatedProbeK caps how many symbols one name may match before it counts as
// common. A name carried by hundreds of symbols says only that the repository
// is large.
const relatedProbeK = 120

// FindRelatedEdits answers "I changed these names here, where else do they
// live" — the question completeness actually turns on.
//
// DECISION(2026-09): the input is the edit, not the task. Three detectors that
// predicted a multi-file change BEFORE the work — from the issue text, from the
// directory tree, from a file's whole vocabulary — all landed at 25-63% recall
// with a 33-36% false-alarm rate, too weak to assert. Scored the same way but
// fed the names an edit actually touched, six blind targets returned at ranks
// 1, 1, 1, 2, 2 and 7. The difference is that the input stopped being a guess:
// after the first edit the change is a fact.
// ASSUMES: a name shared by few symbols marks a real relationship. REVISIT IF:
// a repository's naming is so uniform that rare names are accidental.
func FindRelatedEdits(ctx context.Context, st Store, changed []string, exclude []string, limit int) ([]RelatedFile, error) {
	if limit <= 0 {
		limit = 5
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[normalizeRelatedPath(e)] = true
	}

	names := extractRelatedNames(changed)
	if len(names) == 0 {
		return nil, nil
	}

	type acc struct {
		score   float64
		symbols map[string]bool
		shared  map[string]float64
	}
	byFile := map[int64]*acc{}

	for _, name := range names {
		hits, err := st.SearchByBodyText(ctx, name, relatedProbeK)
		if err != nil {
			return nil, err
		}
		// A name at the cap is not evidence: it is everywhere, so it points
		// nowhere. Rarity is the whole signal here.
		if len(hits) == 0 || len(hits) >= relatedProbeK {
			continue
		}
		weight := 1.0 / float64(len(hits))
		for _, h := range hits {
			a := byFile[h.FileID]
			if a == nil {
				a = &acc{symbols: map[string]bool{}, shared: map[string]float64{}}
				byFile[h.FileID] = a
			}
			a.score += weight
			if h.QualifiedName != "" {
				a.symbols[h.QualifiedName] = true
			}
			if weight > a.shared[name] {
				a.shared[name] = weight
			}
		}
	}
	if len(byFile) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(byFile))
	for id := range byFile {
		ids = append(ids, id)
	}
	paths, err := st.GetFilesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]RelatedFile, 0, len(byFile))
	for id, a := range byFile {
		file := normalizeRelatedPath(paths[id])
		if file == "" || file == "." || skip[file] {
			continue
		}
		rf := RelatedFile{File: file, Score: a.score}
		for s := range a.symbols {
			rf.Symbols = append(rf.Symbols, s)
		}
		sort.Strings(rf.Symbols)
		if len(rf.Symbols) > 3 {
			rf.Symbols = rf.Symbols[:3]
		}
		for n := range a.shared {
			rf.Shared = append(rf.Shared, n)
		}
		sort.Slice(rf.Shared, func(i, j int) bool {
			if a.shared[rf.Shared[i]] != a.shared[rf.Shared[j]] {
				return a.shared[rf.Shared[i]] > a.shared[rf.Shared[j]]
			}
			return rf.Shared[i] < rf.Shared[j]
		})
		if len(rf.Shared) > 4 {
			rf.Shared = rf.Shared[:4]
		}
		out = append(out, rf)
	}
	// Tests are demoted rather than dropped: a change often does belong in one,
	// but a test file matches on the same generic names as everything else it
	// exercises. Measured on django__django-14376, three test files scored into
	// the top five on "password" alone and one outranked the answer, which is
	// the file the agent had actually missed. The offline sweep that validated
	// this ranking excluded tests, so the shipped tool has to order them the
	// same way or its numbers do not transfer.
	sort.Slice(out, func(i, j int) bool {
		ti, tj := relatedTestPath(out[i].File), relatedTestPath(out[j].File)
		if ti != tj {
			return tj
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].File < out[j].File
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// extractRelatedNames pulls identifiers out of whatever the caller sent, which
// may be a pasted diff, a list of names, or a sentence. Diff markers and
// context lines are dropped: only added and removed lines describe the change.
func extractRelatedNames(changed []string) []string {
	seen := map[string]bool{}
	var out []string
	looksLikeDiff := false
	for _, blob := range changed {
		if strings.Contains(blob, "\n+") || strings.Contains(blob, "\n-") || strings.HasPrefix(blob, "diff --git") {
			looksLikeDiff = true
			break
		}
	}
	for _, blob := range changed {
		for _, line := range strings.Split(blob, "\n") {
			if looksLikeDiff {
				if len(line) == 0 || (line[0] != '+' && line[0] != '-') {
					continue
				}
				if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
					continue
				}
				line = line[1:]
			}
			for _, t := range relatedToken.FindAllString(line, -1) {
				// Length is deliberately not a filter: the name that locates the
				// missed file in django__django-14376 is the two-letter key "db",
				// and a noise word like "to" is indistinguishable from it here.
				// Only the store knows which of the two is rare.
				if relatedStopWords[t] || seen[t] {
					continue
				}
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

func normalizeRelatedPath(p string) string {
	return path.Clean(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
}

// relatedTestPath extends the indexer's filename rule with the one dimension it
// does not look at: the directory.
//
// index.IsTestFile is authoritative on naming conventions and is not duplicated
// here. It does miss two shapes that matter for this ranking, both common in
// the corpora this was measured on: a file named plainly "tests.py", and any
// file under a tests/ package. Django uses both — tests/admin_views/tests.py
// scored above the answer on "password" before this existed. Widening
// index.IsTestFile itself would change what the indexer stores and what the
// test floor demotes, moving published benchmark numbers, so the extra
// condition lives here where only this ranking sees it.
func relatedTestPath(p string) bool {
	if walkerTestFile(p) {
		return true
	}
	clean := strings.ToLower(strings.ReplaceAll(p, "\\", "/"))
	base := clean
	if i := strings.LastIndex(clean, "/"); i >= 0 {
		base = clean[i+1:]
		for _, seg := range strings.Split(clean[:i], "/") {
			if seg == "test" || seg == "tests" || seg == "testing" {
				return true
			}
		}
	}
	return base == "tests.py" || base == "test.py"
}
