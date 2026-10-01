package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"slices"
	"strings"
	"time"

	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

const (
	maxTranscriptBytes = 16 << 20
	maxSpeechBytes     = 64 << 20
)

var (
	_ speech.STT = (*Client)(nil)
	_ speech.TTS = (*Client)(nil)
)

var (
	reservedSTTParams = []string{"model", "file", "response_format"}
	reservedTTSParams = []string{"model", "input", "voice", "response_format"}
)

func (c *Client) Transcribe(ctx context.Context, req speech.TranscribeRequest) (speech.Transcript, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", req.Model)
	// verbose_json carries per-segment no-speech scores, used to drop hallucinated silence.
	_ = mw.WriteField("response_format", "verbose_json")
	for _, k := range slices.Sorted(maps.Keys(req.Params)) {
		if slices.Contains(reservedSTTParams, k) {
			return speech.Transcript{}, fmt.Errorf("openaicompat: param %q is reserved", k)
		}
		for _, v := range formValues(req.Params[k]) {
			_ = mw.WriteField(k, v)
		}
	}
	name := req.Filename
	if name == "" {
		name = "audio.wav"
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return speech.Transcript{}, fmt.Errorf("openaicompat: build form: %w", err)
	}
	_, _ = fw.Write(req.Audio)
	if err := mw.Close(); err != nil {
		return speech.Transcript{}, fmt.Errorf("openaicompat: build form: %w", err)
	}

	resp, err := c.post(ctx, "/audio/transcriptions", mw.FormDataContentType(), "application/json", body.Bytes())
	if err != nil {
		return speech.Transcript{}, err
	}
	defer resp.Body.Close()
	var out transcription
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTranscriptBytes)).Decode(&out); err != nil {
		return speech.Transcript{}, fmt.Errorf("openaicompat: decode transcription: %w", err)
	}
	return speech.Transcript{
		Text:     out.text(),
		Language: out.Language,
		Duration: time.Duration(out.Duration * float64(time.Second)),
	}, nil
}

func formValues(v any) []string {
	if list, ok := v.([]any); ok {
		out := make([]string, len(list))
		for i, e := range list {
			out[i] = fmt.Sprint(e)
		}
		return out
	}
	return []string{fmt.Sprint(v)}
}

type transcription struct {
	Text     string  `json:"text"`
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Segments []struct {
		Text         string  `json:"text"`
		AvgLogprob   float64 `json:"avg_logprob"`
		NoSpeechProb float64 `json:"no_speech_prob"`
	} `json:"segments"`
}

// text skips segments Whisper's own decoder treats as silence (no_speech > 0.6 and
// avg_logprob < -1); on silence the model otherwise invents phrases like "Thank you."
func (t transcription) text() string {
	if len(t.Segments) == 0 {
		return strings.Join(strings.Fields(t.Text), " ")
	}
	var b strings.Builder
	for _, s := range t.Segments {
		if s.NoSpeechProb > 0.6 && s.AvgLogprob < -1 {
			continue
		}
		b.WriteString(s.Text)
		b.WriteByte(' ')
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (c *Client) Synthesize(ctx context.Context, req speech.SynthesizeRequest) ([]byte, error) {
	body := map[string]any{"model": req.Model, "input": req.Text, "response_format": "wav"}
	if req.Voice != "" {
		body["voice"] = req.Voice
	}
	for k, v := range req.Params {
		if slices.Contains(reservedTTSParams, k) {
			return nil, fmt.Errorf("openaicompat: param %q is reserved", k)
		}
		body[k] = v
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encode request: %w", err)
	}

	resp, err := c.post(ctx, "/audio/speech", "application/json", "audio/wav", payload)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSpeechBytes+1))
	if err != nil {
		return nil, fmt.Errorf("openaicompat: read speech: %w", err)
	}
	if len(data) > maxSpeechBytes {
		return nil, fmt.Errorf("openaicompat: speech exceeds %d bytes", maxSpeechBytes)
	}
	return data, nil
}
