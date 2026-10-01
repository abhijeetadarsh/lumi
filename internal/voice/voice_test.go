package voice

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

type fakeTTS struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeTTS) Synthesize(_ context.Context, req speech.SynthesizeRequest) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, req.Text)
	return []byte(req.Text), f.err
}

func newTestSpeaker(tts *fakeTTS, maxChars int) (*Speaker, *[]string) {
	var played []string
	s := NewSpeaker(speech.TTSTarget{TTS: tts, MaxChars: maxChars})
	s.play = func(_ context.Context, wav []byte) error {
		played = append(played, string(wav))
		return nil
	}
	return s, &played
}

func TestUtteranceSpeaksStreamedSentences(t *testing.T) {
	tts := &fakeTTS{}
	s, played := newTestSpeaker(tts, 0)
	u := s.Begin(t.Context())
	for _, chunk := range []string{"Hey", " there! How", "'s it going? 😄", " I'm **fine**"} {
		u.Write(chunk)
	}
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	got := ""
	for _, p := range *played {
		got += p + "|"
	}
	// Batching may merge sentences, but the spoken text and order must be intact.
	if want := "Hey there! How's it going? I'm fine"; normalize(got) != want {
		t.Errorf("played %q, want %q", *played, want)
	}
}

func normalize(s string) string {
	out := []rune{}
	for _, r := range s {
		if r == '|' {
			r = ' '
		}
		if r == ' ' && len(out) > 0 && out[len(out)-1] == ' ' {
			continue
		}
		out = append(out, r)
	}
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}

func TestUtteranceRespectsMaxChars(t *testing.T) {
	tts := &fakeTTS{}
	s, _ := newTestSpeaker(tts, 20)
	u := s.Begin(t.Context())
	u.Write("This sentence is certainly longer than twenty characters, so it splits. Ok. ")
	if err := u.Finish(); err != nil {
		t.Fatal(err)
	}
	for _, text := range tts.texts {
		if len([]rune(text)) > 20 {
			t.Errorf("request %q exceeds max_chars", text)
		}
	}
}

func TestUtteranceReportsTTSError(t *testing.T) {
	s, _ := newTestSpeaker(&fakeTTS{err: errors.New("boom")}, 0)
	u := s.Begin(t.Context())
	u.Write("Hello. World. Again. ")
	if err := u.Finish(); err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestSpeakable(t *testing.T) {
	for in, want := range map[string]string{
		"Nice try! 😂":         "Nice try!",
		"**bold** and `code`": "bold and code",
		"👍🏽":                  "",
		"  spaced\n\nout ":     "spaced out",
	} {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplit(t *testing.T) {
	got := split("one two, three four five six", 12)
	want := []string{"one two,", "three four", "five six"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
