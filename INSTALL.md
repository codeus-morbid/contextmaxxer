# Install Contextmaxxer

This guide is safe to hand to a coding agent. Contextmaxxer supports Windows
amd64 and Linux amd64; macOS packages are not published yet.

## 1. Install the binary

Download the archive and `checksums.txt` from the
[latest release](https://github.com/codeus-morbid/contextmaxxer/releases/latest):

- `contextmaxxer_<version>_windows_amd64.zip`
- `contextmaxxer_<version>_linux_amd64.tar.gz`

Verify the archive with `Get-FileHash` on Windows or `sha256sum` on Linux, then
put `contextmaxxer.exe` or `contextmaxxer` on `PATH`. On Linux, make it
executable first:

```text
chmod +x contextmaxxer
contextmaxxer --version
```

To build it yourself instead, follow the source-build instructions in
[README.md](README.md#build-from-source).

## 2. Warm up the local runtime

```text
contextmaxxer warmup
```

The first run downloads several hundred megabytes of embedding, reranking and
ONNX Runtime files with progress output. They are cached per machine; indexing
and querying work offline after warmup.

## 3. Initialize a repository

Run one of these from any directory:

```text
contextmaxxer init --host claude-code /path/to/repo
contextmaxxer init --host cursor /path/to/repo
contextmaxxer init --host codex /path/to/repo
contextmaxxer init /path/to/repo
```

The last form detects installed hosts. `init` indexes the repository and merges
the MCP registration and adoption rule for each selected host. Existing config
entries are preserved and repeated runs are idempotent.

For a large repository, add `--fast` to make lexical, symbol and graph search
available first. Embeddings are backfilled when the MCP server starts.

## 4. Restart and approve

Two actions remain manual:

1. Restart the agent app or CLI so it reloads the MCP configuration.
2. Approve the Contextmaxxer MCP server if the host prompts for approval.

## 5. Verify the correct index

Run the query from the indexed repository root:

```text
cd /path/to/repo
contextmaxxer query "where is the entry point"
```

Check the `index:` path printed on stderr. Ranked symbols from that repository
confirm that the local index and models are working.

## Network and data boundary

Source files, indexes and queries stay on the machine during normal operation.
Network access is used to download the model weights and the ONNX Runtime, both
from URLs pinned in the source. Every one of those downloads is checked against
a SHA256 recorded in the binary, and a mismatch aborts the install rather than
warning: the runtime archives are native libraries this process loads and
executes, so an unverified one is arbitrary code. An artifact with no recorded
hash is refused for the same reason; `CONTEXTMAXXER_ALLOW_UNVERIFIED_DOWNLOAD=1`
overrides that if you are bringing up a platform this build does not pin yet.

Optional LLM enrichment contacts only the endpoint explicitly supplied by the
user.

Feedback logging can be disabled with `--feedback-log none`. Before sharing a
log, create a redacted export:

```text
contextmaxxer feedback export . -redact
```
