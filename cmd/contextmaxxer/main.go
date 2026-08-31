package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/codeus-morbid/contextmaxxer/internal/app"
	"github.com/codeus-morbid/contextmaxxer/internal/cli"
)

var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	flag.Usage = usage
	cfg := app.ConfigFromFlags()

	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// Only an explicitly given -index is forwarded to a subcommand that owns a
	// flag of the same name; forwarding the default would override the
	// subcommand's own default with an identical one for no reason.
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "index" {
			cfg.IndexPathSet = true
		}
	})

	if *showVersion {
		fmt.Printf("contextmaxxer %s (commit=%s, built=%s)\n", version, commit, buildTime)
		return 0
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return 1
	}
	subcommand := args[0]
	subArgs := args[1:]

	// The discovery-gate hook runs on every gated tool call: handle it before any
	// app/DB/model init and let it control the exact exit code (2 blocks).
	if subcommand == "hook" {
		return cli.RunHook(subArgs)
	}

	a, err := app.New(*cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	a.SetSubArgs(subArgs)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := a.Run(ctx, subcommand); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: contextmaxxer [flags] <subcommand> [args]\n\n")
	fmt.Fprintf(os.Stderr, "Subcommands: warmup, init [path], index [path], enrich [path], query, mcp, feedback export\n\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}
