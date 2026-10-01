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
func (c *Client) Synthesize(ctx context.Context, req speech.SynthesizeRequest) ([]byte, error) {
    // --------------------------------------------------------------------
    // Build the JSON payload
    // --------------------------------------------------------------------
    payload := synthRequest{}
    payload.Input.Text = req.Text
    if req.Voice != "" {
        // A full voice name ("en-US-Standard-B") must carry its language code too,
        // so derive it from the BCP-47 prefix when the caller gave only a name.
        if i := strings.IndexByte(req.Voice, '-'); i > 0 {
            payload.Voice.Name = req.Voice
            payload.Voice.LanguageCode = req.Voice[:i]
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
