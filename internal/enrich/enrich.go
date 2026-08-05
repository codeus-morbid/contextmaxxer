// Package enrich generates one-sentence purpose summaries for indexed code
// symbols using an OpenAI-compatible chat completion endpoint. Summaries are
// written to .contextmaxxer/synthetic-docs.json and merged into symbol
// docstrings at index time, closing the vocabulary gap between intent-style
// queries ("why") and code text ("how").
//
// DECISION(2026-06): one client for both local and cloud backends — Ollama,
// LM Studio and DeepSeek all expose the OpenAI chat completions API, so the
// backend is just BaseURL+Model+APIKey. Local (Ollama, qwen2.5-coder:3b) is
// the default: ~2GB on disk, runs CPU-only on an average dev machine.
// REVISIT IF: a local runtime without an OpenAI-compatible endpoint must be
// supported.
package enrich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

const (
	defaultTimeout  = 120 * time.Second
	maxBodyChars    = 1400
	maxSummaryChars = 300
	// DECISION(2026-06): the prompt demands symbol-specific concrete detail
	// (generic summaries made sibling methods MORE similar in embedding space)
	// but must NOT contain the word "distinguish" — qwen echoed the instruction
	// verbatim into 10-24% of summaries ("...distinguishing it from similar
	// symbols by..."), polluting embeddings. cleanSummary additionally strips
	// any such echo tails. REVISIT IF: echo phrases reappear with a new model.
	systemPrompt = "You document code symbols for a semantic code search index. " +
		"Reply with ONLY one English sentence (max 35 words) describing what this symbol does and why it exists. " +
		"Include the concrete specifics of this exact symbol: key inputs, outputs, conditions or side effects. " +
		"Never use filler like 'handles requests', 'manages data' or 'this function', and never compare it to other symbols. " +
		"No code, no markdown, no quotes, no preamble."
)

type Config struct {
	BaseURL string // OpenAI-compatible endpoint, e.g. http://localhost:11434/v1
	Model   string
	APIKey  string // empty for local backends
	Timeout time.Duration
	Log     *slog.Logger
}

type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger
}

func NewClient(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: cfg.Timeout},
		log:  cfg.Log,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float32       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// SummarizeSymbol asks the model for a one-sentence purpose summary of a
// symbol. Returns a cleaned single-line summary.
func (c *Client) SummarizeSymbol(ctx context.Context, filePath string, sym store.Symbol) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "File: %s\n", filePath)
	fmt.Fprintf(&b, "Kind: %s\n", sym.Kind)
	fmt.Fprintf(&b, "Symbol: %s\n", sym.QualifiedName)
	if sym.Signature != "" {
		fmt.Fprintf(&b, "Signature: %s\n", sym.Signature)
	}
	if sym.Docstring != "" {
		fmt.Fprintf(&b, "Existing comment: %s\n", truncate(sym.Docstring, 300))
	}
	if sym.BodyExcerpt != "" {
		fmt.Fprintf(&b, "Body:\n%s\n", truncate(sym.BodyExcerpt, maxBodyChars))
	}
	b.WriteString("\nOne-sentence purpose summary:")

	raw, err := c.chat(ctx, []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		return "", err
	}
	summary := cleanSummary(raw)
	if summary == "" {
		return "", fmt.Errorf("model returned empty summary for %s", sym.QualifiedName)
	}
	return summary, nil
}

func (c *Client) chat(ctx context.Context, messages []chatMessage) (string, error) {
	reqBody, err := json.Marshal(chatRequest{
		Model:       c.cfg.Model,
		Messages:    messages,
		Temperature: 0.2,
		MaxTokens:   120,
		Stream:      false,
	})
	if err != nil {
		return "", fmt.Errorf("marshal chat request: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("new chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read chat response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat endpoint %s returned %d: %s", url, resp.StatusCode, truncate(string(body), 300))
	}

	var parsed chatResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse chat response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("chat error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("chat response has no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}

// echoMarkers are instruction-echo fragments models append to summaries;
// everything from the marker to the end of the sentence is noise.
var echoMarkers = []string{
	"distinguish",
	"setting it apart",
	"sets it apart",
	"set it apart",
	"unlike similar",
	"unlike other",
	"compared to similar",
	"compared to other",
}

// cleanSummary normalizes model output to one plain sentence-like line and
// strips instruction-echo tails.
func cleanSummary(s string) string {
	s = strings.TrimSpace(s)
	// Some models wrap the answer in quotes or emit a label prefix.
	s = strings.Trim(s, "\"'`")
	for _, prefix := range []string{"Summary:", "summary:", "Purpose:", "purpose:"} {
		s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	s = StripEchoTail(s)
	s = strings.TrimSpace(s)
	if len(s) > maxSummaryChars {
		cut := s[:maxSummaryChars]
		if sp := strings.LastIndexByte(cut, ' '); sp > maxSummaryChars/2 {
			cut = cut[:sp]
		}
		s = cut + " ..."
	}
	return s
}

// StripEchoTail removes an instruction-echo clause ("..., distinguishing it
// from similar symbols by ...") and everything after it. Exported so existing
// sidecar files can be sanitized without regeneration.
func StripEchoTail(s string) string {
	lower := strings.ToLower(s)
	cut := -1
	for _, m := range echoMarkers {
		if idx := strings.Index(lower, m); idx >= 0 && (cut == -1 || idx < cut) {
			cut = idx
		}
	}
	if cut == -1 {
		return s
	}
	head := strings.TrimSpace(s[:cut])
	// Drop dangling connector words left before the removed clause
	// ("...; it", "..., which", "... and").
	for {
		head = strings.TrimRight(head, ",;:- ")
		lowerHead := strings.ToLower(head)
		trimmed := false
		for _, conn := range []string{" it", " which", " that", " this", " and", " thereby", " thus", " while", " also"} {
			if strings.HasSuffix(lowerHead, conn) {
				head = strings.TrimSpace(head[:len(head)-len(conn)])
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
	}
	if head == "" {
		return s
	}
	if !strings.HasSuffix(head, ".") {
		head += "."
	}
	return head
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + " ..."
}
