package openaicompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

func TestTranscribeSendsFormAndDropsSilentSegments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		got := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			got[k] = v[0]
		}
		want := map[string]string{"model": "stt-model", "response_format": "verbose_json", "language": "en", "temperature": "0"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("form (-want +got):\n%s", diff)
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("file: %v", err)
		}
		if b, _ := io.ReadAll(f); string(b) != "RIFFdata" || h.Filename != "speech.wav" {
			t.Errorf("file = %q (%s)", b, h.Filename)
		}
		_, _ = io.WriteString(w, `{"text":" I goes home. Thank you.","language":"English","duration":2.5,"segments":[
			{"text":" I goes home.","avg_logprob":-0.2,"no_speech_prob":0.01},
			{"text":" Thank you.","avg_logprob":-1.4,"no_speech_prob":0.9}]}`)
	}))
	defer srv.Close()

	tr, err := newTestClient(t, srv.URL, -1).Transcribe(t.Context(), speech.TranscribeRequest{
		Model:    "stt-model",
		Audio:    []byte("RIFFdata"),
		Filename: "speech.wav",
		Params:   map[string]any{"language": "en", "temperature": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "I goes home." || tr.Language != "English" || tr.Duration.Seconds() != 2.5 {
		t.Errorf("transcript = %+v", tr)
	}
}

func TestTranscribeRejectsReservedParams(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1", -1)
	_, err := c.Transcribe(t.Context(), speech.TranscribeRequest{Model: "m", Params: map[string]any{"response_format": "text"}})
	if err == nil {
		t.Fatal("want error for reserved param")
	}
}

func TestSynthesizeReturnsWAV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"model": "tts-model", "input": "Hello there.", "voice": "v1", "response_format": "wav", "speed": 1.1}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("body (-want +got):\n%s", diff)
		}
		_, _ = io.WriteString(w, "RIFF....WAVE")
	}))
	defer srv.Close()

	wav, err := newTestClient(t, srv.URL, -1).Synthesize(t.Context(), speech.SynthesizeRequest{
		Model: "tts-model", Voice: "v1", Text: "Hello there.", Params: map[string]any{"speed": 1.1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(wav) != "RIFF....WAVE" {
		t.Errorf("wav = %q", wav)
	}
}
