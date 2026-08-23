// Command rspbreak prices a find_context response by section.
//
// Every token we send is meant to move the agent toward one of three
// decisions: cite the answer, expand it, or re-query. Trimming the payload
// without knowing where the tokens actually go is guesswork — measured on
// cockroach the tool spent 92K tokens against grep's 59K, and that ratio says
// nothing about which part to cut.
//
// It parses the real markdown an agent receives (never the JSON encoding,
// which no agent reads) and splits it into what the sections cost. Repeated
// boilerplate is counted separately from the text it decorates, because a
// disclaimer that is informative once is pure repetition by its fifth copy.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/codeus-morbid/contextmaxxer/internal/evalharness"
	"github.com/codeus-morbid/contextmaxxer/internal/selfcases"
)

// Section names, ordered as they appear in a response.
const (
	secRequestID  = "request_id"
	secHeader     = "result_header"
	secExpandHint = "  ^ expand hint"
	secTeaser     = "compact_teaser"
	secCode       = "code"
	secLineNums   = "  ^ line numbers"
	secGraph      = "graph_refs"
	secGraphBoil  = "  ^ path_status disclaimer"
	secCallsite   = "  ^ inline callsite evidence"
	secCompanions = "companions"
	secFlow       = "flow_context"
	secNote       = "confidence_note"
	secFraming    = "fences_and_blanks"
)

// Sub-sections are counted inside their parent, so they must not be added to
// the total twice.
var subSection = map[string]bool{
	secExpandHint: true,
	secLineNums:   true,
	secGraphBoil:  true,
}

var (
	headerRe    = regexp.MustCompile(`^\d+\. \*\*`)
	lineNumRe   = regexp.MustCompile(`^(\d+\t)`)
	expandRe    = regexp.MustCompile(` \[excerpt [^\]]*\]`)
	graphBoiler = " (path_status=static_unverified; verify branch/dispatch)"
	feedbackTag = " (pass to record_feedback)"
	// The disclaimer is appended to the LABEL, so the line reads
	// "callers (path_status=...): ..." and a plain "callers:" prefix misses it.
	graphLineRe = regexp.MustCompile(`^(callers|callees|tests|siblings)`)
	callsiteRe  = regexp.MustCompile(` \[callsite: [^\]]*\]`)
)

func main() {
	bin := flag.String("bin", "dist/contextmaxxer.exe", "binary to serve the requests")
	indexPath := flag.String("index", "./.contextmaxxer/index.db", "index db")
	n := flag.Int("n", 40, "queries to sample from the index")
	maxResults := flag.Int("max", 5, "max_results per call (the served default)")
	mode := flag.String("mode", "", "output mode: answer (default), minimal, explore")
	minDoc := flag.Int("min-doc", 40, "minimum docstring length for a sampled query")
	dump := flag.Bool("dump", false, "print the first response verbatim")
	flag.Parse()

	cases, _, err := selfcases.Sample(*indexPath, selfcases.Options{
		N: *n, MinDoc: *minDoc, Kinds: "function,method",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sample queries:", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "no documented symbols to build queries from")
		os.Exit(1)
	}

	srv, err := evalharness.Start(*bin, *indexPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start server:", err)
		os.Exit(1)
	}
	defer srv.Stop()

	totals := map[string]int{}
	responses := 0
	for i, c := range cases {
		md, err := srv.FindMarkdown(c.Paraphrase(), *maxResults, *mode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", c.Qualified, err)
			os.Exit(1)
		}
		if *dump && i == 0 {
			fmt.Println("----- first response verbatim -----")
			fmt.Println(md)
			fmt.Println("----- end -----")
		}
		for sec, tok := range breakdown(md) {
			totals[sec] += tok
		}
		responses++
	}

	report(totals, responses)
}

// breakdown classifies every byte of one response.
func breakdown(md string) map[string]int {
	out := map[string]int{}
	inCode := false
	for _, line := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(line, "```"):
			out[secFraming] += tokens(line)
			inCode = !inCode
		case inCode:
			out[secCode] += tokens(line)
			if m := lineNumRe.FindString(line); m != "" {
				out[secLineNums] += tokens(m)
			}
		case strings.TrimSpace(line) == "":
			out[secFraming] += tokens(line)
		case strings.HasPrefix(line, "req:"):
			out[secRequestID] += tokens(line)
		case headerRe.MatchString(line):
			out[secHeader] += tokens(line)
			if m := expandRe.FindString(line); m != "" {
				out[secExpandHint] += tokens(m)
			}
			// A compact result carries its teaser on the header line.
			if idx := strings.Index(line, " — "); idx >= 0 {
				out[secTeaser] += tokens(line[idx:])
				out[secHeader] -= tokens(line[idx:])
			}
		case graphLineRe.MatchString(line):
			out[secGraph] += tokens(line)
			if strings.Contains(line, graphBoiler) {
				out[secGraphBoil] += tokens(graphBoiler)
			}
			for _, ev := range callsiteRe.FindAllString(line, -1) {
				out[secCallsite] += tokens(ev)
			}
		case strings.HasPrefix(line, "companions:"):
			out[secCompanions] += tokens(line)
		case strings.HasPrefix(line, "flow:"):
			out[secFlow] += tokens(line)
		case strings.HasPrefix(line, "note:"):
			out[secNote] += tokens(line)
		default:
			out[secFraming] += tokens(line)
		}
	}
	if strings.Contains(md, feedbackTag) {
		out[secRequestID+"  ^ feedback nudge"] += tokens(feedbackTag)
	}
	return out
}

// tokens matches internal/retrieve's estimator so these numbers are comparable
// with the budget the packer enforces.
func tokens(text string) int { return (len(text) + 3) / 4 }

func report(totals map[string]int, responses int) {
	total := 0
	for sec, tok := range totals {
		if !isSub(sec) {
			total += tok
		}
	}
	fmt.Printf("responses=%d  avg_tokens=%d\n\n", responses, total/max(responses, 1))

	names := make([]string, 0, len(totals))
	for sec := range totals {
		names = append(names, sec)
	}
	// Sub-sections sort directly under their parent by sharing its prefix.
	sort.SliceStable(names, func(i, j int) bool {
		if isSub(names[i]) != isSub(names[j]) {
			return totals[parentOf(names[i])] > totals[parentOf(names[j])]
		}
		return totals[names[i]] > totals[names[j]]
	})
	sort.SliceStable(names, func(i, j int) bool {
		return totals[parentOf(names[i])] > totals[parentOf(names[j])]
	})

	fmt.Printf("%-28s %10s %8s\n", "section", "avg tokens", "share")
	for _, sec := range names {
		share := ""
		if !isSub(sec) {
			share = fmt.Sprintf("%.1f%%", 100*float64(totals[sec])/float64(max(total, 1)))
		}
		fmt.Printf("%-28s %10.1f %8s\n", sec, float64(totals[sec])/float64(max(responses, 1)), share)
	}
}

func isSub(sec string) bool {
	if subSection[sec] {
		return true
	}
	return strings.Contains(sec, "  ^ ")
}

// parentOf groups a sub-section with the section it is measured inside.
func parentOf(sec string) string {
	switch sec {
	case secLineNums:
		return secCode
	case secGraphBoil, secCallsite:
		return secGraph
	case secExpandHint:
		return secHeader
	}
	if i := strings.Index(sec, "  ^ "); i >= 0 {
		return sec[:i]
	}
	return sec
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
