#!/usr/bin/env bash
# Smoke test: the built binary starts and speaks MCP over stdio.
#
# Build and unit tests are blind to the ONNX Runtime ABI. The hermetic suite
# never loads a real model, so a Go binding that requests a newer ORT API than
# the pinned native runtime provides compiles clean, passes every test, and
# then dies at startup with "Error setting ORT API base". This runs the real
# binary and asserts that it answers a client.
#
# An index is required: `mcp` rejects a v0 content format before it touches
# ONNX, so a handshake without one would fail for the wrong reason and prove
# nothing. A two-symbol throwaway project indexes in about 150 ms.
set -euo pipefail

bin="${1:-}"
if [ -z "$bin" ]; then
  if [ -f ".task/build/contextmaxxer.exe" ]; then
    bin=".task/build/contextmaxxer.exe"
  else
    bin=".task/build/contextmaxxer"
  fi
fi
[ -f "$bin" ] || { echo "handshake: binary not found: $bin" >&2; exit 1; }
bin="$(cd "$(dirname "$bin")" && pwd)/$(basename "$bin")"

run() { if command -v timeout >/dev/null 2>&1; then timeout "$@"; else shift; "$@"; fi; }

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

cat > "$workdir/main.go" <<'GO'
package main

import "fmt"

func Greet(name string) string { return fmt.Sprintf("hi %s", name) }

func main() { fmt.Println(Greet("world")) }
GO
printf 'module handshakeprobe\n\ngo 1.25\n' > "$workdir/go.mod"

cat > "$workdir/in.jsonl" <<'JSON'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"ci-handshake","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
JSON

dump() {
  echo "--- server stderr ---" >&2
  cat "$workdir/err.log" 2>/dev/null >&2 || true
  echo "--- server stdout ---" >&2
  cat "$workdir/out.jsonl" 2>/dev/null >&2 || true
}

echo "handshake: indexing a throwaway project"
if ! ( cd "$workdir" && run 600 "$bin" index . > "$workdir/index.log" 2>&1 ); then
  echo "handshake: indexing failed" >&2
  cat "$workdir/index.log" >&2 || true
  exit 1
fi

echo "handshake: running $bin mcp"
status=0
( cd "$workdir" && run 300 "$bin" mcp < "$workdir/in.jsonl" > "$workdir/out.jsonl" 2> "$workdir/err.log" ) || status=$?
if [ "$status" -ne 0 ]; then
  echo "handshake: server exited with status $status" >&2
  dump
  exit 1
fi

init="$(grep '"id":1' "$workdir/out.jsonl" || true)"
tools="$(grep '"id":2' "$workdir/out.jsonl" || true)"

if [ -z "$init" ] || ! printf '%s' "$init" | grep -q '"protocolVersion"'; then
  echo "handshake: initialize returned no protocolVersion" >&2
  dump
  exit 1
fi
if [ -z "$tools" ]; then
  echo "handshake: tools/list returned nothing" >&2
  dump
  exit 1
fi

for tool in find_context expand_context continue_context record_feedback; do
  if ! printf '%s' "$tools" | grep -q "\"name\":\"$tool\""; then
    echo "handshake: tool $tool missing from tools/list" >&2
    dump
    exit 1
  fi
done

if ! printf '%s' "$tools" | grep -q '"inputSchema"'; then
  echo "handshake: tools/list carries no inputSchema — schema serialization broke" >&2
  dump
  exit 1
fi

echo "handshake: ok —$(printf '%s' "$tools" | grep -o '"name":"[a-z_]*"' | sed 's/"name":"/ /;s/"//' | sort -u | tr -d '\n')"
