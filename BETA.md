# Contextmaxxer public beta

The beta is for testing one question: can a coding agent reach the right symbol
with fewer searches and file reads on a real repository?

Start with [INSTALL.md](INSTALL.md), then use Contextmaxxer on a repository you
already know well. Large codebases are the most informative; small repositories
often do not expose a meaningful advantage over grep.

## What to try

Ask mechanism-level questions whose answers cross files or call boundaries:

- where is the retry budget decided?
- what writes the final index metadata?
- how does a request reach the ranking pipeline?
- which callers can trigger this fallback?

Use the agent normally. The host configuration written by `contextmaxxer init`
asks it to call `find_context` before broad grep/glob exploration.

## What useful feedback looks like

For a retrieval miss, include:

- the natural-language query;
- the symbol or file you expected;
- which result positions were useful, if any;
- the Contextmaxxer version and repository language mix;
- whether the index was built with `--fast` or full embeddings.

Setup friction, crashes, surprising network activity and wrong-index selection
are equally valuable. Do not post proprietary source or credentials in public
issues.

## Optional feedback export

The local feedback log can contain queries, paths, symbol names and numeric
ranking features, but not function bodies. Inspect it before sharing. A redacted
export hashes the identifying strings while preserving ranking signals:

```text
contextmaxxer feedback export . -redact
```

Logging is optional and can be disabled by adding `--feedback-log none` to the
MCP server arguments.

## Enrichment experiment

`contextmaxxer init` emits `.contextmaxxer/pending-docs.json`. A coding agent can
turn each entry into one concrete English sentence describing the exact
symbol's purpose, then merge the mapping into
`.contextmaxxer/synthetic-docs.json` and reindex:

```text
contextmaxxer -force index .
```

Treat enrichment as an A/B experiment: keep the original query set, compare
before and after, and report regressions as well as wins.

## Current limits

- Release packages target Windows amd64 and Linux amd64.
- First warmup downloads several hundred megabytes.
- Initial indexing is CPU-heavy.
- Generated and highly dynamic code can produce incomplete call edges.
- The public self-eval is a regression suite, not an independent benchmark; see
  [docs/evaluation.md](docs/evaluation.md).
