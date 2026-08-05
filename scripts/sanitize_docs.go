//go:build ignore

// One-off sanitizer: applies enrich.StripEchoTail to existing
// synthetic-docs.json sidecars (regenerating thousands of summaries is much
// more expensive than stripping the echo tails in place).
//
// Usage: go run scripts/sanitize_docs.go <sidecar.json> [...]
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/codeus-morbid/contextmaxxer/internal/enrich"
)

func main() {
	for _, path := range os.Args[1:] {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		var docs map[string]string
		if err := json.Unmarshal(data, &docs); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		changed := 0
		for k, v := range docs {
			if cleaned := enrich.StripEchoTail(v); cleaned != v {
				docs[k] = cleaned
				changed++
			}
		}
		out, err := json.MarshalIndent(docs, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, out, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Printf("%s: cleaned %d/%d summaries\n", path, changed, len(docs))
	}
}
