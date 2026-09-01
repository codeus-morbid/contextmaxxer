// Package evalharness drives the shipped binary's `mcp` subcommand over
// JSON-RPC stdio, exactly like an agent host does. Extracted from cmd/mcpeval
// so every served-path evaluation tool (mcpeval, selfsweep, giteval) measures
// the same stack users run instead of the retrieval library.
package evalharness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os/exec"
)

type rpcResp struct {
	ID     int `json:"id"`
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}

type toolPayload struct {
	Symbols []struct {
		Name      string  `json:"name"`
		File      string  `json:"file"`
		Score     float32 `json:"score"`
		Relevance float32 `json:"relevance"`
		Lines     string  `json:"lines"`
		Visible   string  `json:"visible_lines"`
		Callers   []struct {
			Name string `json:"name"`
		} `json:"callers"`
		Callees []struct {
			Name string `json:"name"`
		} `json:"callees"`
	} `json:"symbols"`
	RetrievalHealth *struct {
		Confidence  string  `json:"confidence"`
		TopScoreGap float32 `json:"top_score_gap"`
	} `json:"retrieval_health"`
}

// Result is one find_context response, reduced to what evaluation needs.
type Result struct {
	Names []string
	// Files is each symbol's path, relative to the indexed root. Region-level
	// benchmarks score (file, line-start, line-end) tuples, so the path is not
	// optional there the way it is for name-matching probes.
	Files     []string
	Scores    []float32
	Relevance []float32
	// Lines is each symbol's full span ("120-380"); Visible is the span the
	// response actually shows after evidence trimming. They differ exactly
	// when the answer may have been trimmed away, which ranking metrics
	// cannot see — see cmd/deepprobe.
	Lines   []string
	Visible []string
	// Callers and Callees are the graph refs shown for each result. They are
	// what makes a chain followable without a second search, so cmd/chainprobe
	// measures exactly them.
	Callers    [][]string
	Callees    [][]string
	Confidence string  // "" when the server omitted retrieval_health
	TopGap     float32 // relative top1-top2 gap as the server computed it
}

// Server is one spawned `<bin> mcp --index <path>` process.
type Server struct {
	cmd        *exec.Cmd
	in         *json.Encoder
	out        *bufio.Scanner
	seq        int
	seedK      int
	fullBodies int
	rerankK    int
	skipIntent bool
}

// Start spawns the server and completes the MCP initialize handshake.
func Start(bin, indexPath string, extraArgs ...string) (*Server, error) {
	// extraArgs exists so a probe can vary ONE served knob (say, the reranker)
	// without forking the harness; with none passed the server takes the release
	// defaults, which is what a host gets.
	cmd := exec.Command(bin, append([]string{"mcp", "--index", indexPath}, extraArgs...)...)
	cmd.Stderr = nil // server logs are noise here
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	s := &Server{cmd: cmd, in: json.NewEncoder(stdin), out: sc}

	if err := s.send(map[string]any{
		"jsonrpc": "2.0", "id": s.next(), "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "evalharness", "version": "0"},
		},
	}); err != nil {
		return nil, err
	}
	if _, err := s.recv(s.seq); err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) next() int { s.seq++; return s.seq }

func (s *Server) send(v any) error { return s.in.Encode(v) }

func (s *Server) recv(wantID int) (*rpcResp, error) {
	for s.out.Scan() {
		line := s.out.Bytes()
		var r rpcResp
		if err := json.Unmarshal(line, &r); err != nil {
			continue // notifications/log lines
		}
		if r.ID == wantID {
			return &r, nil
		}
	}
	if err := s.out.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("server closed before response %d", wantID)
}

// SetSkipIntent disables the intent ranker for every subsequent call on this
// server (experiment knob).
func (s *Server) SetSkipIntent(v bool) { s.skipIntent = v }

// SeedK, when non-zero, overrides the server's seed-pool size for every
// subsequent call on this server (experiment knob; 0 = server default).
func (s *Server) SetSeedK(k int) { s.seedK = k }

// SetFullBodies controls how many top results keep their full body (-1 = all).
func (s *Server) SetFullBodies(n int) { s.fullBodies = n }

// SetRerankK sizes the cross-encoder pool; it must be >= max_results or the
// tail of a long response never reaches the reranker.
func (s *Server) SetRerankK(k int) { s.rerankK = k }

// FindContext calls the find_context tool and returns ranked qualified names.
func (s *Server) FindContext(query string, maxResults int) ([]string, error) {
	res, err := s.Find(query, maxResults)
	if err != nil {
		return nil, err
	}
	return res.Names, nil
}

// FindContextHealth additionally returns the retrieval_health confidence
// ("" when the server omitted the block, i.e. answered confidently).
func (s *Server) FindContextHealth(query string, maxResults int) ([]string, string, error) {
	res, err := s.Find(query, maxResults)
	if err != nil {
		return nil, "", err
	}
	return res.Names, res.Confidence, nil
}

// Find returns the full reduced response (names, scores, health).
func (s *Server) Find(query string, maxResults int) (Result, error) {
	id := s.next()
	args := map[string]any{
		"query":       query,
		"max_results": maxResults,
		// json: harnesses score ranking, not encoding; the md and
		// json paths share the retrieval result.
		"format": "json",
	}
	if s.fullBodies != 0 {
		args["full_body_results"] = s.fullBodies
	}
	if s.rerankK > 0 {
		args["rerank_k"] = s.rerankK
	}
	if s.seedK > 0 {
		args["seed_k"] = s.seedK
	}
	if s.skipIntent {
		args["skip_intent"] = true
	}
	if err := s.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{
			"name":      "find_context",
			"arguments": args,
		},
	}); err != nil {
		return Result{}, err
	}
	r, err := s.recv(id)
	if err != nil {
		return Result{}, err
	}
	if r.Result.IsError || len(r.Result.Content) == 0 {
		return Result{}, fmt.Errorf("tool error for %q", query)
	}
	var p toolPayload
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &p); err != nil {
		return Result{}, fmt.Errorf("parse payload: %w", err)
	}
	out := Result{
		Names:     make([]string, len(p.Symbols)),
		Files:     make([]string, len(p.Symbols)),
		Scores:    make([]float32, len(p.Symbols)),
		Relevance: make([]float32, len(p.Symbols)),
		Lines:     make([]string, len(p.Symbols)),
		Visible:   make([]string, len(p.Symbols)),
		Callers:   make([][]string, len(p.Symbols)),
		Callees:   make([][]string, len(p.Symbols)),
	}
	for i, sym := range p.Symbols {
		out.Names[i] = sym.Name
		out.Files[i] = sym.File
		out.Scores[i] = sym.Score
		out.Relevance[i] = sym.Relevance
		out.Lines[i] = sym.Lines
		out.Visible[i] = sym.Visible
		for _, c := range sym.Callers {
			out.Callers[i] = append(out.Callers[i], c.Name)
		}
		for _, c := range sym.Callees {
			out.Callees[i] = append(out.Callees[i], c.Name)
		}
	}
	if p.RetrievalHealth != nil {
		out.Confidence = p.RetrievalHealth.Confidence
		out.TopGap = p.RetrievalHealth.TopScoreGap
	}
	return out, nil
}

// Stop kills the server process.
func (s *Server) Stop() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
}

// FindMarkdown returns the response exactly as an agent host receives it. The
// other helpers parse the JSON encoding, which is the wrong shape for judging
// what an agent actually reads.
func (s *Server) FindMarkdown(query string, maxResults int, mode string) (string, error) {
	id := s.next()
	args := map[string]any{
		"query":       query,
		"max_results": maxResults,
		"format":      "md",
	}
	if mode != "" {
		args["mode"] = mode
	}
	if s.seedK > 0 {
		args["seed_k"] = s.seedK
	}
	if err := s.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": "find_context", "arguments": args},
	}); err != nil {
		return "", err
	}
	r, err := s.recv(id)
	if err != nil {
		return "", err
	}
	if r.Result.IsError || len(r.Result.Content) == 0 {
		return "", fmt.Errorf("tool error for %q", query)
	}
	return r.Result.Content[0].Text, nil
}

// FindMarkdownFull is FindMarkdown with evidence trimming disabled, so bodies
// come back whole. It stands in for expand_context in single-shot harnesses:
// expand_context addresses a rank inside a live server session, and a harness
// that spawns a process per call has no session to address.
func (s *Server) FindMarkdownFull(query string, maxResults int, mode string) (string, error) {
	id := s.next()
	args := map[string]any{
		"query":                query,
		"max_results":          maxResults,
		"format":               "md",
		"preserve_full_bodies": true,
	}
	if mode != "" {
		args["mode"] = mode
	}
	if err := s.send(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": "find_context", "arguments": args},
	}); err != nil {
		return "", err
	}
	r, err := s.recv(id)
	if err != nil {
		return "", err
	}
	if r.Result.IsError || len(r.Result.Content) == 0 {
		return "", fmt.Errorf("tool error for %q", query)
	}
	return r.Result.Content[0].Text, nil
}
