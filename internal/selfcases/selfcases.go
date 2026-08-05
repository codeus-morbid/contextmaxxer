// Package selfcases samples label-free retrieval cases from an index: a
// documented symbol plus the query built from its own docstring. Shared by
// cmd/selfsweep (scores them) and cmd/confcal (uses them as positives whose
// answer definitely exists).
package selfcases

import (
	"database/sql"
	"strings"

	_ "modernc.org/sqlite"
)

// Case is one symbol and the queries derived from its docstring.
type Case struct {
	Name      string // bare symbol name
	Qualified string // qualified name, the retrieval target
	Sentence  string // first docstring sentence, comment markers stripped
}

// Paraphrase is Sentence with the leading symbol name removed, so lexical
// search cannot match the answer by name alone.
func (c Case) Paraphrase() string { return StripLeadingName(c.Sentence, c.Name) }

// Options controls sampling.
type Options struct {
	N      int    // sample size (stride over all qualifying symbols)
	MinDoc int    // minimum docstring length
	Kinds  string // comma-separated kinds; "all" disables the filter
}

// DECISION(2026-07): behavioral kinds only by default. Const/type docs in the
// Go convention restate the name ("DefaultAuthMode is the default
// authentication mode"), so a name-stripped query is a subject-less predicate
// no agent would type — those cases measure grammar, not retrieval.
func Sample(indexPath string, opt Options) ([]Case, int, error) {
	if opt.MinDoc == 0 {
		opt.MinDoc = 40
	}
	if opt.Kinds == "" {
		opt.Kinds = "function,method"
	}
	db, err := sql.Open("sqlite", "file:"+indexPath+"?mode=ro")
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()

	kindFilter, args := "", []any{opt.MinDoc, opt.MinDoc}
	if opt.Kinds != "all" {
		var placeholders []string
		for _, k := range strings.Split(opt.Kinds, ",") {
			placeholders = append(placeholders, "?")
			args = append(args, strings.TrimSpace(k))
		}
		kindFilter = " AND s.kind IN (" + strings.Join(placeholders, ",") + ")"
	}

	rows, err := db.Query(`
		SELECT s.name, s.qualified_name, s.docstring FROM symbols s
		WHERE length(s.docstring) >= ?
		  AND s.docstring NOT IN (
		    SELECT docstring FROM symbols WHERE length(docstring) >= ?
		    GROUP BY docstring HAVING count(*) > 1)`+kindFilter+`
		ORDER BY s.qualified_name`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var all []Case
	for rows.Next() {
		var c Case
		var doc string
		if err := rows.Scan(&c.Name, &c.Qualified, &doc); err != nil {
			return nil, 0, err
		}
		c.Sentence = FirstSentence(doc)
		if len(c.Sentence) < opt.MinDoc {
			continue
		}
		// A sentence that loses too much on name-stripping has no semantic
		// payload beyond the name.
		if len(c.Paraphrase()) < 25 {
			continue
		}
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	total := len(all)
	if opt.N <= 0 || total <= opt.N {
		return all, total, nil
	}
	// Deterministic stride sample, stable across runs and spread across
	// packages (input is sorted by qualified name).
	stride := float64(total) / float64(opt.N)
	out := make([]Case, 0, opt.N)
	for i := 0; i < opt.N; i++ {
		out = append(out, all[int(float64(i)*stride)])
	}
	return out, total, nil
}

// FirstSentence extracts the leading prose sentence and strips the comment
// syntax the indexer stored with it. Queries must not contain "//" — the
// stored docstring does, which would hand lexical search a literal substring
// of the answer.
func FirstSentence(doc string) string {
	doc = strings.TrimSpace(doc)
	if i := strings.Index(doc, "\n\n"); i > 0 {
		doc = doc[:i]
	}
	fields := strings.Fields(doc)
	clean := fields[:0]
	for _, f := range fields {
		switch f {
		case "//", "///", "/*", "/**", "*", "*/", `"""`, "'''", "#":
			continue
		}
		clean = append(clean, strings.TrimPrefix(strings.TrimPrefix(f, `"""`), "'''"))
	}
	doc = strings.Join(clean, " ")
	if i := strings.Index(doc, ". "); i > 0 {
		doc = doc[:i+1]
	}
	if len(doc) > 220 {
		doc = doc[:220]
	}
	return doc
}

// StripLeadingName removes the Go-doc leading symbol name ("Foo returns..."
// -> "returns...").
func StripLeadingName(sentence, name string) string {
	fields := strings.Fields(sentence)
	if len(fields) > 1 && strings.EqualFold(fields[0], name) {
		return strings.Join(fields[1:], " ")
	}
	return sentence
}
