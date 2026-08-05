// Package negcases supplies absent-concept queries for false-confidence
// measurement, and — crucially — verifies per repo that each concept really
// is absent before it counts as a negative.
//
// DECISION(2026-07): a global "these concepts don't exist anywhere" list is
// invalid by construction. Measured on cockroach: gRPC bidirectional
// streaming, reconcile loops and batch-index retries all exist there, so 4 of
// 12 "negatives" had correct answers and the tool was penalized for being
// right. Verification is lexical (SQL LIKE over the index) and therefore
// independent of the semantic ranking under test: if a distinctive term
// appears nowhere in the indexed text, the tool cannot legitimately answer.
package negcases

import (
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Case is one plausible-sounding query plus the terms whose total absence
// from the index makes it a valid negative.
type Case struct {
	Query string
	Terms []string
}

// Default is the candidate pool. Each entry names a technology or domain
// concept specific enough that its absence is checkable by a literal term.
var Default = []Case{
	{"kafka consumer group rebalance listener", []string{"kafka"}},
	{"stripe payment webhook signature verification", []string{"stripe"}},
	{"graphql schema resolver for mutations", []string{"graphql"}},
	{"kubernetes operator reconcile loop", []string{"kubernetes", "reconcile"}},
	{"webrtc peer connection ice negotiation", []string{"webrtc"}},
	{"elasticsearch bulk indexing with retries", []string{"elasticsearch"}},
	{"blockchain wallet transaction signing", []string{"blockchain"}},
	{"smtp email delivery retry queue", []string{"smtp"}},
	{"grpc bidirectional streaming handler", []string{"grpc"}},
	{"saml single sign-on assertion parsing", []string{"saml"}},
	{"terraform state locking backend", []string{"terraform"}},
	{"mqtt topic subscription with qos", []string{"mqtt"}},
	{"opengl shader compilation pipeline", []string{"opengl", "shader"}},
	{"bluetooth device pairing handshake", []string{"bluetooth"}},
	{"pdf page rendering to bitmap", []string{"pdf"}},
	{"midi note event scheduling", []string{"midi"}},
	{"lidar point cloud downsampling", []string{"lidar", "point cloud"}},
	{"payroll tax withholding calculation", []string{"payroll", "withholding"}},
}

// Verify returns the subset of cases whose every term is absent from the
// index, plus the dropped ones with the term that disqualified them.
func Verify(indexPath string, cases []Case) (valid []Case, dropped map[string]string, err error) {
	db, err := sql.Open("sqlite", "file:"+indexPath+"?mode=ro")
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	// Lexical presence over everything the index knows about a symbol, plus
	// file paths — the widest net the tool itself could draw on.
	const q = `SELECT 1 FROM symbols s JOIN files f ON f.id = s.file_id
		WHERE lower(s.qualified_name) LIKE ? OR lower(s.docstring) LIKE ?
		   OR lower(s.signature) LIKE ? OR lower(s.body_excerpt) LIKE ?
		   OR lower(f.path) LIKE ? LIMIT 1`

	dropped = make(map[string]string)
	for _, c := range cases {
		hitTerm := ""
		for _, term := range c.Terms {
			pat := "%" + strings.ToLower(term) + "%"
			var one int
			switch err := db.QueryRow(q, pat, pat, pat, pat, pat).Scan(&one); err {
			case nil:
				hitTerm = term
			case sql.ErrNoRows:
				// term absent — keep checking the rest
			default:
				return nil, nil, fmt.Errorf("verify %q: %w", term, err)
			}
			if hitTerm != "" {
				break
			}
		}
		if hitTerm == "" {
			valid = append(valid, c)
		} else {
			dropped[c.Query] = hitTerm
		}
	}
	return valid, dropped, nil
}
