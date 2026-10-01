// Package llm defines the provider-agnostic language model interface.
package llm

import (
	"context"
	"iter"
	"strings"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role
	Content string
}

type Request struct {
	Model       string
	Messages    []Message
	Temperature *float64
	MaxTokens   int
	// Params are provider-specific body fields (e.g. reasoning_effort, top_p); they override standard fields.
	Params map[string]any
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Chunk struct {
	Text         string
	FinishReason string
	Usage        *Usage
}

type Response struct {
	Text         string
	FinishReason string
	Usage        Usage
}

type Capabilities struct {
	Tools    bool
	JSONMode bool
}

type LLM interface {
	Chat(ctx context.Context, req Request) (Response, error)
	// Stream reports request-level failures before iteration; callers must range the result to release the connection.
	Stream(ctx context.Context, req Request) (iter.Seq2[Chunk, error], error)
	Caps() Capabilities
}

// Collect drains a stream into a single Response.
func Collect(stream iter.Seq2[Chunk, error]) (Response, error) {
	var (
		b    strings.Builder
		resp Response
	)
	for c, err := range stream {
		if err != nil {
			return Response{}, err
		}
		b.WriteString(c.Text)
		if c.FinishReason != "" {
			resp.FinishReason = c.FinishReason
		}
		if c.Usage != nil {
			resp.Usage = *c.Usage
		}
	}
	resp.Text = b.String()
	return resp, nil
}
