package main

// What does the same hop cost without the graph?
//
// chainprobe already prices the tool arm exactly: 36 of 39 next hops arrive
// attached to the previous answer, so following the chain costs no second
// search. That number means nothing on its own — the question an agent's budget
// actually asks is how much the alternative costs, and the alternative is a
// text search.
//
// This prices it deterministically, with no agent and no model in the loop:
// grep the snapshot for the next hop's identifier, sort the matching files the
// way a person reads grep output, and count how far down the target sits. That
// count is how many files an agent opens before it reaches what one graph ref
// would have handed it.
//
// The model being assumed is stated rather than hidden: an agent reads grep
// output in order and opens files until it finds the definition. Real agents
// skim paths and sometimes guess right first, so treat reads-to-target as an
// upper bound on the text-search arm and the match count as the haystack it is
// choosing from. What is NOT assumed is anything about our own arm: whether the
// hop was on the page is measured, not modelled.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// grepIndex is one repository scanned once: identifier -> files containing it.
// Scanning per hop would re-read a large snapshot dozens of times for no gain.
type grepIndex struct {
	root  string
	cache map[string][]string
	files []string
	texts map[string]string
}

var grepExt = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true, ".py": true, ".java": true, ".rs": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true,
	".hpp": true, ".hh": true, ".cs": true, ".rb": true, ".php": true,
	".kt": true, ".kts": true, ".scala": true, ".sc": true,
}

func grepSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "target", ".contextmaxxer":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// newGrepIndex reads the searchable files once. Memory is traded for honesty:
// every hop is then measured against exactly the same corpus.
func newGrepIndex(root string) (*grepIndex, error) {
	g := &grepIndex{root: root, cache: map[string][]string{}, texts: map[string]string{}}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && grepSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !grepExt[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 1<<20 {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = strings.ReplaceAll(rel, "\\", "/")
		g.files = append(g.files, rel)
		g.texts[rel] = string(data)
		return nil
	})
	// Path order, which is the order grep prints and a person reads.
	sort.Strings(g.files)
	return g, err
}

// matches returns the files containing an identifier, in the order grep prints
// them.
func (g *grepIndex) matches(ident string) []string {
	if hit, ok := g.cache[ident]; ok {
		return hit
	}
	var out []string
	for _, f := range g.files {
		if strings.Contains(g.texts[f], ident) {
			out = append(out, f)
		}
	}
	g.cache[ident] = out
	return out
}

// shortName is the identifier a person would actually grep for: the last
// segment of a qualified name.
func shortName(qname string) string {
	if i := strings.LastIndexAny(qname, "./"); i >= 0 && i+1 < len(qname) {
		return qname[i+1:]
	}
	return qname
}

// readsToTarget is how many files an agent opens before reaching the one that
// holds the target, reading grep output top to bottom. It returns the count and
// whether the target file was in the output at all — a grep that never contains
// the answer is a different failure from a grep that buries it, and averaging
// the two together would hide both.
func (g *grepIndex) readsToTarget(ident, targetFile string) (reads int, found bool) {
	targetFile = strings.ReplaceAll(targetFile, "\\", "/")
	hits := g.matches(ident)
	for i, f := range hits {
		if f == targetFile || strings.HasSuffix(f, "/"+targetFile) || strings.HasSuffix(targetFile, "/"+f) {
			return i + 1, true
		}
	}
	return len(hits), false
}
