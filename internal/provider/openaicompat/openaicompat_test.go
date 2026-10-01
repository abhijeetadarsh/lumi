package openaicompat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
)

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	return b.String()
}

func newTestClient(t *testing.T, url string, retries int) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: url + "/v1", APIKey: "sk-test", MaxRetries: retries})
	if err != nil {
		t.Fatal(err)
	}
	c.baseDelay = time.Millisecond
	return c
}

func TestChatStreamsChunksAndUsage(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-test" {
			t.Errorf("Authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keep-alive\n\n"+sse(
			`{"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			`[DONE]`,
		))
	}))
	defer srv.Close()

	temp := 0.3
	resp, err := newTestClient(t, srv.URL, -1).Chat(t.Context(), llm.Request{
		Model:       "m1",
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		Temperature: &temp,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := llm.Response{Text: "Hello", FinishReason: "stop", Usage: llm.Usage{InputTokens: 7, OutputTokens: 2}}
	if diff := cmp.Diff(want, resp); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
	wantReq := chatRequest{
		Model:         "m1",
		Messages:      []chatMessage{{Role: "user", Content: "hi"}},
		Temperature:   &temp,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if diff := cmp.Diff(wantReq, got); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}

func TestStreamRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "overloaded", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, sse(`{"choices":[{"delta":{"content":"ok"}}]}`, `[DONE]`))
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv.URL, 2).Chat(t.Context(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "ok" || calls.Load() != 2 {
		t.Errorf("text = %q, calls = %d; want ok, 2", resp.Text, calls.Load())
	}
}

func TestStreamDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, 3).Chat(t.Context(), llm.Request{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Message != "bad key" || calls.Load() != 1 {
		t.Errorf("got %+v after %d calls", apiErr, calls.Load())
	}
}

func TestStreamSurfacesErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sse(`{"choices":[{"delta":{"content":"par"}}]}`, `{"error":{"message":"overloaded"}}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, -1).Chat(t.Context(), llm.Request{Model: "m"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "overloaded" {
		t.Errorf("err = %v, want stream APIError", err)
	}
}

func TestParamsMergeIntoBody(t *testing.T) {
	temp := 0.3
	body, err := encodeRequest(llm.Request{
		Model:       "m",
		Temperature: &temp,
		Params:      map[string]any{"reasoning_effort": "default", "temperature": 0.6},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["reasoning_effort"] != "default" || got["temperature"] != 0.6 || got["stream"] != true {
		t.Errorf("body = %v", got)
	}

	if _, err := encodeRequest(llm.Request{Model: "m", Params: map[string]any{"stream": false}}); err == nil {
		t.Error("reserved param accepted, want error")
	}
}

func TestNewRejectsInvalidBaseURL(t *testing.T) {
	for _, u := range []string{"", "localhost:11434", "ftp://x", "http://"} {
		if _, err := New(Options{BaseURL: u}); err == nil {
			t.Errorf("New(%q) succeeded, want error", u)
		}
	}
}
