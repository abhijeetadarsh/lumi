// Package googletts implements speech.TTS by calling Google Cloud Text-to-Speech.
// It works with either an API key (passed as a query param) or a service‑account
// JSON via Application Default Credentials.
package googletts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2/google"

	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

// ---------------------------------------------------------------------------
// 1.  Client that implements speech.TTS
// ---------------------------------------------------------------------------

type Client struct {
	baseURL string
	apiKey  string // optional – if empty we use ADC token
}

// New creates a client. The resolver will call this with the provider’s
// BaseURL and the resolved API key (if any).
func New(baseURL, apiKey string) *Client {
	// Google expects the URL without a trailing slash.
	baseURL = strings.TrimSuffix(baseURL, "/")
	return &Client{baseURL: baseURL, apiKey: apiKey}
}

// synthRequest mirrors the JSON payload described in the Google REST docs.
type synthRequest struct {
	Input struct {
		Text string `json:"text,omitempty"`
	} `json:"input"`
	Voice struct {
		LanguageCode string `json:"languageCode,omitempty"`
		Name         string `json:"name,omitempty"`
	} `json:"voice"`
	AudioConfig struct {
		AudioEncoding string  `json:"audioEncoding"`
		Pitch         float64 `json:"pitch,omitempty"`
		SpeakingRate  float64 `json:"speakingRate,omitempty"`
	} `json:"audioConfig"`
}

// Synthesize implements speech.TTS. It returns a WAV (LINEAR16) byte slice.
// languageOf extracts the language tag from a Google voice name such as
// "en-US-Chirp3-HD-Kore" or "cmn-CN-Wavenet-A". The tag is the leading run of
// subtags (language, optional script, optional region); everything after it is
// the voice family and name. It returns false when there is no such tag.
func languageOf(voice string) (string, bool) {
	parts := strings.Split(voice, "-")
	tag := []string{parts[0]}
	for _, p := range parts[1:] {
		// A subtag continues the language tag only while it is a script (4 letters,
		// e.g. Hans) or a region (2 letters / 3 digits, e.g. US or 419). The next
		// family such as "Chirp3" or "Wavenet" ends it.
		if len(p) == 4 || (len(p) == 2 && isAlpha(p)) || (len(p) == 3 && isDigits(p)) {
			tag = append(tag, p)
			continue
		}
		break
	}
	if len(tag) == 1 && !strings.Contains(voice, "-") {
		return "", false
	}
	return strings.Join(tag, "-"), true
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			if r < 'A' || r > 'Z' {
				return false
			}
		}
	}
	return len(s) > 0
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func (c *Client) Synthesize(ctx context.Context, req speech.SynthesizeRequest) ([]byte, error) {
	// --------------------------------------------------------------------
	// Build the JSON payload
	// --------------------------------------------------------------------
	payload := synthRequest{}
	payload.Input.Text = req.Text
	if req.Voice != "" {
		// A named voice must carry a matching language code, and Google rejects a
		// bare region ("en" from "en-US-...") when the voice declares "en-US".
		// Take the region from the name itself rather than the first subtag.
		if lang, ok := languageOf(req.Voice); ok {
			payload.Voice.Name = req.Voice
			payload.Voice.LanguageCode = lang
		} else {
			payload.Voice.LanguageCode = req.Voice
		}
	}
	payload.AudioConfig.AudioEncoding = "LINEAR16"
	// Optional extra params (e.g. pitch, speakingRate)
	if p, ok := req.Params["pitch"]; ok {
		if v, ok2 := p.(float64); ok2 {
			payload.AudioConfig.Pitch = v
		}
	}
	if p, ok := req.Params["speakingRate"]; ok {
		if v, ok2 := p.(float64); ok2 {
			payload.AudioConfig.SpeakingRate = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal Google TTS payload: %w", err)
	}

	// --------------------------------------------------------------------
	// Build HTTP request (Google uses POST /v1/text:synthesize)
	// --------------------------------------------------------------------
	googleURL, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", c.baseURL, err)
	}
	googleURL.Path = "/v1/text:synthesize"

	// If an API key is supplied we add it as a query parameter.
	if c.apiKey != "" {
		q := googleURL.Query()
		q.Set("key", c.apiKey)
		googleURL.RawQuery = q.Encode()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, googleURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Google request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// --------------------------------------------------------------------
	// Authentication – either API key (already in URL) or ADC token
	// --------------------------------------------------------------------
	if c.apiKey == "" {
		ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
		if err != nil {
			return nil, fmt.Errorf("Google ADC token source: %w", err)
		}
		token, err := ts.Token()
		if err != nil {
			return nil, fmt.Errorf("Google ADC token: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}

	// --------------------------------------------------------------------
	// Perform request
	// --------------------------------------------------------------------
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Google TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Google TTS error %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Google returns JSON with a base64‑encoded audioContent field.
	var gResp struct {
		AudioContent string `json:"audioContent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		return nil, fmt.Errorf("decode Google response: %w", err)
	}
	// Decode base64
	data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(gResp.AudioContent)))
	if err != nil {
		return nil, fmt.Errorf("decode base64 audio: %w", err)
	}
	return data, nil
}
