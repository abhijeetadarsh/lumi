// Package openaicompat implements llm.LLM, speech.STT and speech.TTS for any server
// speaking the OpenAI-compatible protocol (/chat/completions, /audio/transcriptions,
// /audio/speech), hosted or local.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
)

const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	maxBackoff        = 8 * time.Second
	maxRetryAfter     = 30 * time.Second
	maxSSELineBytes   = 1 << 20
	maxErrorBodyBytes = 4 << 10
)

type Options struct {
	BaseURL    string // API root; endpoint paths such as /chat/completions are appended
	APIKey     string // empty for unauthenticated local servers
	HTTPClient *http.Client
	MaxRetries int // 0 = default, negative = no retries
}

type Client struct {
	baseURL    string
	apiKey     string
	http       *http.Client
	maxRetries int
	baseDelay  time.Duration
}

var _ llm.LLM = (*Client)(nil)

func New(o Options) (*Client, error) {
	u, err := url.Parse(o.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("openaicompat: invalid base URL %q", o.BaseURL)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = defaultHTTPClient()
	}
	retries := o.MaxRetries
	switch {
	case retries == 0:
		retries = defaultMaxRetries
	case retries < 0:
		retries = 0
	}
	return &Client{
		baseURL:    strings.TrimRight(o.BaseURL, "/"),
		apiKey:     o.APIKey,
		http:       hc,
		maxRetries: retries,
		baseDelay:  defaultBaseDelay,
	}, nil
}

func defaultHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 2 * time.Minute
	// No client-wide timeout: streams are long-lived and bounded by the request context.
	return &http.Client{Transport: t}
}

// Caps reports protocol-level support; per-model overrides arrive with tool calling.
func (c *Client) Caps() llm.Capabilities {
	return llm.Capabilities{Tools: true, JSONMode: true}
}

func (c *Client) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	stream, err := c.Stream(ctx, req)
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Collect(stream)
}

func (c *Client) Stream(ctx context.Context, req llm.Request) (iter.Seq2[llm.Chunk, error], error) {
	payload, err := encodeRequest(req)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encode request: %w", err)
	}
	resp, err := c.post(ctx, "/chat/completions", "application/json", "text/event-stream", payload)
	if err != nil {
		return nil, err
	}
	return func(yield func(llm.Chunk, error) bool) {
		defer resp.Body.Close()
		readSSE(resp.Body, yield)
	}, nil
}

func (c *Client) post(ctx context.Context, path, contentType, accept string, payload []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("openaicompat: build request: %w", err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", accept)
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.http.Do(req)
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}
		if attempt >= c.maxRetries || !retryable(resp, err) {
			if err != nil {
				return nil, fmt.Errorf("openaicompat: %w", err)
			}
			defer resp.Body.Close()
			return nil, readAPIError(resp)
		}

		delay := c.backoff(attempt, resp)
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
			resp.Body.Close()
		}
		slog.Debug("retrying provider request", "path", path, "attempt", attempt+1, "delay", delay, "status", status, "err", err)

		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

func (c *Client) backoff(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			return min(time.Duration(s)*time.Second, maxRetryAfter)
		}
	}
	d := min(c.baseDelay<<attempt, maxBackoff)
	return d/2 + rand.N(d/2+1)
}

// APIError is a non-2xx response or an error event inside the stream (StatusCode 0).
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.StatusCode == 0 {
		return "openaicompat: stream error: " + e.Message
	}
	return fmt.Sprintf("openaicompat: HTTP %d: %s", e.StatusCode, e.Message)
}

func readAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	msg := strings.TrimSpace(string(body))
	var env struct {
		Error *errorBody `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error != nil && env.Error.Message != "" {
		msg = env.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg}
}

func readSSE(r io.Reader, yield func(llm.Chunk, error) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELineBytes)
	for sc.Scan() {
		data, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:"))
		if !ok {
			continue // comments, event:, id:, blank separators
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if string(data) == "[DONE]" {
			return
		}

		var ev streamChunk
		if err := json.Unmarshal(data, &ev); err != nil {
			yield(llm.Chunk{}, fmt.Errorf("openaicompat: decode stream chunk: %w", err))
			return
		}
		if ev.Error != nil {
			yield(llm.Chunk{}, &APIError{Message: ev.Error.Message})
			return
		}

		var ch llm.Chunk
		if len(ev.Choices) > 0 {
			ch.Text = ev.Choices[0].Delta.Content
			if fr := ev.Choices[0].FinishReason; fr != nil {
				ch.FinishReason = *fr
			}
		}
		if ev.Usage != nil {
			ch.Usage = &llm.Usage{InputTokens: ev.Usage.PromptTokens, OutputTokens: ev.Usage.CompletionTokens}
		}
		if ch == (llm.Chunk{}) {
			continue
		}
		if !yield(ch, nil) {
			return
		}
	}
	if err := sc.Err(); err != nil {
		yield(llm.Chunk{}, fmt.Errorf("openaicompat: read stream: %w", err))
	}
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
}

// reservedParams are body fields the adapter controls; Params may not set them.
var reservedParams = []string{"model", "messages", "stream", "stream_options"}

func encodeRequest(req llm.Request) ([]byte, error) {
	b, err := json.Marshal(toWire(req))
	if err != nil || len(req.Params) == 0 {
		return b, err
	}
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, err
	}
	for k, v := range req.Params {
		if slices.Contains(reservedParams, k) {
			return nil, fmt.Errorf("param %q is reserved", k)
		}
		body[k] = v
	}
	return json.Marshal(body)
}

func toWire(req llm.Request) chatRequest {
	msgs := make([]chatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = chatMessage{Role: string(m.Role), Content: m.Content}
	}
	return chatRequest{
		Model:         req.Model,
		Messages:      msgs,
		Temperature:   req.Temperature,
		MaxTokens:     req.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
}
