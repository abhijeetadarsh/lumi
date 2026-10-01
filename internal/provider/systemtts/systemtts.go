// Package systemtts implements speech.TTS with the operating system's built-in
// voices: free, offline, no API key. Currently Windows (SAPI) only.
package systemtts

import (
	"context"
	"sync"

	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

var _ speech.TTS = (*Client)(nil)

type Client struct {
	once sync.Once
	reqs chan request
}

type request struct {
	req  speech.SynthesizeRequest
	resp chan result
}

type result struct {
	wav []byte
	err error
}

func New() *Client { return &Client{reqs: make(chan request)} }

// Synthesize returns a WAV file. Requests are served one at a time by a single
// engine thread; a cancelled ctx abandons the wait, not the synthesis in flight.
func (c *Client) Synthesize(ctx context.Context, req speech.SynthesizeRequest) ([]byte, error) {
	c.once.Do(func() { go serve(c.reqs) })
	r := request{req: req, resp: make(chan result, 1)}
	select {
	case c.reqs <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case res := <-r.resp:
		return res.wav, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// intParam reads a numeric param; YAML may decode it as int or float64.
func intParam(params map[string]any, key string) (int, bool) {
	switch v := params[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}
