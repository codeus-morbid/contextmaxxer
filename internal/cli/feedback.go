package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/feedback"
)

// RunFeedback handles `contextmaxxer feedback <verb>`. Currently the only verb
// is `export`, which copies the feedback log (optionally redacted) into a single
// file ready to hand back to the maintainer for ranker training.
func RunFeedback(ctx context.Context, args []string, log *slog.Logger) error {
	if len(args) == 0 || args[0] != "export" {
		return fmt.Errorf("usage: contextmaxxer feedback export [path] [-redact] [-o out.jsonl]")
	}
	return runFeedbackExport(ctx, args[1:], log)
}

func runFeedbackExport(_ context.Context, args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("feedback export", flag.ContinueOnError)
	redact := fs.Bool("redact", false, "hash queries, file paths and symbol names; drop free-text fields (numeric features and labels are preserved)")
	outPath := fs.String("o", "", "output path (default: <root>/.contextmaxxer/feedback-export[.redacted].jsonl)")
	fs.SetOutput(os.Stderr)
	// Go's flag package stops at the first positional, so flags placed after the
	// path (e.g. `feedback export . -redact`) would be ignored. Parse in a loop,
	// consuming one positional at a time, to accept flags in any position.
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}

	root := "."
	if len(positional) > 0 {
		root = positional[0]
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	inPath := filepath.Join(absRoot, ".contextmaxxer", "feedback.jsonl")
	// The log rotates at a size cap, so the retained history is the previous
	// generation followed by the current one. Reading only the current file
	// would silently export a fraction of what is on disk.
	inPaths := []string{feedback.RotatedPath(inPath), inPath}
	present := inPaths[:0]
	for _, p := range inPaths {
		if _, err := os.Stat(p); err == nil {
			present = append(present, p)
		}
	}
	inPaths = present
	if len(inPaths) == 0 {
		return fmt.Errorf("open feedback log (%s): no such file", inPath)
	}

	dest := *outPath
	if dest == "" {
		name := "feedback-export.jsonl"
		if *redact {
			name = "feedback-export.redacted.jsonl"
		}
		dest = filepath.Join(absRoot, ".contextmaxxer", name)
	}
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create export: %w", err)
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	defer w.Flush()

	var (
		retrievals, feedbacks, skipped int
		withFeatures, withLabels       int
		earliest, latest               time.Time
	)
	track := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
		if t.After(latest) {
			latest = t
		}
	}

	scanFeedbackFile := func(in io.Reader) error {
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // large lines: candidate arrays can be big
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var peek struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal([]byte(line), &peek); err != nil {
				skipped++
				continue
			}
			switch peek.Event {
			case "retrieval":
				var ev feedback.RetrievalEvent
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					skipped++
					continue
				}
				track(ev.Time)
				retrievals++
				for _, c := range ev.Candidates {
					if len(c.Features) > 0 {
						withFeatures++
						break
					}
				}
				if *redact {
					redactRetrieval(&ev)
				}
				if err := writeJSONL(w, ev); err != nil {
					return err
				}
			case "feedback":
				var ev feedback.FeedbackEvent
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					skipped++
					continue
				}
				track(ev.Time)
				feedbacks++
				if len(ev.SelectedSymbols) > 0 || len(ev.RejectedSymbols) > 0 || ev.Outcome != "" {
					withLabels++
				}
				if *redact {
					redactFeedback(&ev)
				}
				if err := writeJSONL(w, ev); err != nil {
					return err
				}
			default:
				skipped++
			}
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("read feedback log: %w", err)
		}
		return nil
	}

	for _, p := range inPaths {
		in, err := os.Open(p)
		if err != nil {
			return fmt.Errorf("open feedback log (%s): %w", p, err)
		}
		err = scanFeedbackFile(in)
		in.Close()
		if err != nil {
			return err
		}
	}

	span := "n/a"
	if !earliest.IsZero() {
		span = fmt.Sprintf("%s … %s", earliest.Format("2006-01-02"), latest.Format("2006-01-02"))
	}
	fmt.Printf("Exported -> %s%s\n", dest, redactSuffix(*redact))
	fmt.Printf("  retrievals: %d (%d with feature vectors)\n", retrievals, withFeatures)
	fmt.Printf("  feedback:   %d (%d with usefulness labels)\n", feedbacks, withLabels)
	fmt.Printf("  span:       %s\n", span)
	if skipped > 0 {
		fmt.Printf("  skipped:    %d unparseable lines\n", skipped)
	}
	if feedbacks == 0 {
		fmt.Printf("  note: no feedback events — the agent never called record_feedback, so there are no\n")
		fmt.Printf("        ranking labels yet. Retrieval rows are still useful but cannot supervise a ranker.\n")
	}
	_ = log
	return nil
}

func redactRetrieval(ev *feedback.RetrievalEvent) {
	ev.Query = hashField(ev.Query)
	for i := range ev.Candidates {
		ev.Candidates[i].File = hashField(ev.Candidates[i].File)
		ev.Candidates[i].QualifiedName = hashField(ev.Candidates[i].QualifiedName)
		ev.Candidates[i].Why = "" // free text may quote code/identifiers
	}
}

func redactFeedback(ev *feedback.FeedbackEvent) {
	ev.Query = hashField(ev.Query)
	ev.Note = ""
	for i := range ev.SelectedSymbols {
		ev.SelectedSymbols[i] = hashField(ev.SelectedSymbols[i])
	}
	for i := range ev.RejectedSymbols {
		ev.RejectedSymbols[i] = hashField(ev.RejectedSymbols[i])
	}
}

// hashField deterministically hashes a value so the same path/symbol maps to the
// same token everywhere (preserving the retrieval↔feedback join) without
// revealing its contents. Empty stays empty.
func hashField(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return "h:" + hex.EncodeToString(sum[:])[:12]
}

func writeJSONL(w *bufio.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal export event: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write export: %w", err)
	}
	return nil
}

func redactSuffix(redact bool) string {
	if redact {
		return " (redacted)"
	}
	return ""
}
