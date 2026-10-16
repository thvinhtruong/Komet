// Package llm is a minimal OpenAI-compatible chat client. The same client
// talks to llama-server, Ollama, LM Studio and cloud providers, which is what
// lets the router treat local and cloud as interchangeable tiers.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Messages    []Message
	MaxTokens   int
	Temperature float64
	// JSONSchema asks for grammar-constrained output (llama-server supports
	// this natively, so a small model cannot emit malformed JSON).
	JSONSchema map[string]any
	// JSONObject asks for plain JSON mode (most cloud providers).
	JSONObject bool
	Logprobs   bool
	// CachePrompt is a llama-server extension that reuses the KV cache of a
	// shared prompt prefix (our fixed system prompt) across requests.
	CachePrompt bool
}

type Response struct {
	Text         string
	Logprobs     []float64
	PromptTokens int
	OutputTokens int
	Latency      time.Duration
	Model        string
}

type Client struct {
	BaseURL string // e.g. http://127.0.0.1:8080/v1
	APIKey  string
	Model   string
	HTTP    *http.Client
}

func New(baseURL, apiKey, model string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

type wireRequest struct {
	Model          string    `json:"model,omitempty"`
	Messages       []Message `json:"messages"`
	Temperature    float64   `json:"temperature"`
	MaxTokens      int       `json:"max_tokens,omitempty"`
	Stream         bool      `json:"stream"`
	Logprobs       bool      `json:"logprobs,omitempty"`
	ResponseFormat any       `json:"response_format,omitempty"`
	CachePrompt    *bool     `json:"cache_prompt,omitempty"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Logprobs *struct {
			Content []struct {
				Logprob float64 `json:"logprob"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// HTTPError is returned for non-2xx responses.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// Chat sends one completion request. If the server rejects the optional
// features (structured output, logprobs) with a 4xx, it retries once
// without them, so the client works against servers that lack them.
func (c *Client) Chat(ctx context.Context, req Request) (*Response, error) {
	resp, err := c.do(ctx, req, true)
	if he, ok := err.(*HTTPError); ok && he.Status >= 400 && he.Status < 500 && he.Status != 401 && he.Status != 403 && he.Status != 429 {
		if req.JSONSchema != nil || req.JSONObject || req.Logprobs || req.CachePrompt {
			return c.do(ctx, req, false)
		}
	}
	return resp, err
}

func (c *Client) do(ctx context.Context, req Request, extras bool) (*Response, error) {
	w := wireRequest{
		Model:       c.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if extras {
		w.Logprobs = req.Logprobs
		switch {
		case req.JSONSchema != nil:
			w.ResponseFormat = map[string]any{
				"type":        "json_schema",
				"json_schema": map[string]any{"name": "answer", "schema": req.JSONSchema},
			}
		case req.JSONObject:
			w.ResponseFormat = map[string]any{"type": "json_object"}
		}
		if req.CachePrompt {
			t := true
			w.CachePrompt = &t
		}
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	start := time.Now()
	hresp, err := c.HTTP.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer hresp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if hresp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, &HTTPError{Status: hresp.StatusCode, Body: msg}
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if wr.Error != nil {
		return nil, fmt.Errorf("server error: %s", wr.Error.Message)
	}
	if len(wr.Choices) == 0 {
		return nil, fmt.Errorf("server returned no choices")
	}
	out := &Response{
		Text:         wr.Choices[0].Message.Content,
		PromptTokens: wr.Usage.PromptTokens,
		OutputTokens: wr.Usage.CompletionTokens,
		Latency:      time.Since(start),
		Model:        wr.Model,
	}
	if out.Model == "" {
		out.Model = c.Model
	}
	if lp := wr.Choices[0].Logprobs; lp != nil {
		for _, t := range lp.Content {
			out.Logprobs = append(out.Logprobs, t.Logprob)
		}
	}
	// Some servers omit usage; estimate ~4 chars per token so the ledger
	// still has a number.
	if out.PromptTokens == 0 {
		n := 0
		for _, m := range req.Messages {
			n += len(m.Content)
		}
		out.PromptTokens = n/4 + 1
	}
	if out.OutputTokens == 0 {
		out.OutputTokens = len(out.Text)/4 + 1
	}
	return out, nil
}
