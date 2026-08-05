// Command embprobe is a 30-second semantic sanity check for an embedding
// model in OUR runtime (tokenizer + pooling + prefixes + ORT): it embeds a
// few fixed code snippets and two NL queries and prints the cosine matrix.
// A healthy integration shows the matching snippet winning by a wide margin;
// broken pooling/prefix/EOS shows flat or shuffled scores. Use it before
// paying for a full corebench run on a new model.
//
//	go run ./cmd/embprobe <model-name>
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/codeus-morbid/contextmaxxer/internal/embed"
)

func cos(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s // vectors are L2-normalized
}

func main() {
	model := os.Args[1]
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e, err := embed.NewOnnxEmbedder(context.Background(), embed.Config{ModelName: model, Log: log})
	if err != nil {
		panic(err)
	}
	defer e.Close()

	docs := []string{
		"func add(a, b int) int { return a + b }",
		"def multiply(x, y):\n    return x * y",
		"SELECT name, email FROM users WHERE active = true",
		"class HttpServer { listen(port) { this.sock.bind(port) } }",
	}
	dv, err := e.Embed(context.Background(), docs)
	if err != nil {
		panic(err)
	}
	queries := []string{
		"function that adds two numbers",
		"start an http server listening on a port",
	}
	qv, err := e.EmbedQueries(context.Background(), queries)
	if err != nil {
		panic(err)
	}
	for qi, q := range queries {
		fmt.Printf("Q: %s\n", q)
		for di, d := range docs {
			fmt.Printf("  %.4f  %.40s\n", cos(qv[qi], dv[di]), d)
		}
	}
}
