// Package speech defines provider-agnostic speech-to-text and text-to-speech interfaces.
package speech

import (
	"context"
	"time"
)

type TranscribeRequest struct {
	Model    string
	Audio    []byte // an encoded audio file, e.g. WAV
	Filename string // tells the server the container format
	// Params are provider-specific form fields (e.g. language, prompt, temperature).
	Params map[string]any
}

type Transcript struct {
	Text     string
	Language string
	Duration time.Duration
}

type SynthesizeRequest struct {
	Model  string
	Voice  string
	Text   string
	Params map[string]any
}

type STT interface {
	Transcribe(ctx context.Context, req TranscribeRequest) (Transcript, error)
}

type TTS interface {
	// Synthesize returns a WAV file.
	Synthesize(ctx context.Context, req SynthesizeRequest) ([]byte, error)
}

// STTTarget is a resolved speech-to-text config: a client plus the model to call on it.
type STTTarget struct {
	STT    STT
	Model  string
	Params map[string]any
}

// TTSTarget is a resolved text-to-speech config.
type TTSTarget struct {
	TTS      TTS
	Model    string
	Voice    string
	MaxChars int // longest input the model accepts per request; 0 = unlimited
	Params   map[string]any
}
