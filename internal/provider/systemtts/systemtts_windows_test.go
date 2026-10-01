//go:build windows

package systemtts

import (
	"strings"
	"testing"

	"github.com/abhijeetadarsh/lumi/internal/audio"
	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

func TestSynthesizeWithInstalledVoice(t *testing.T) {
	c := New()
	wav, err := c.Synthesize(t.Context(), speech.SynthesizeRequest{Text: "Hello there. <not xml>", Params: map[string]any{"rate": 1}})
	if err != nil {
		t.Skipf("no usable system voice: %v", err)
	}
	f, pcm, err := audio.DecodeWAV(wav)
	if err != nil {
		t.Fatal(err)
	}
	if d := f.Duration(pcm); d.Seconds() < 0.5 {
		t.Errorf("audio too short: %v", d)
	}
}

func TestUnknownVoiceListsInstalled(t *testing.T) {
	_, err := New().Synthesize(t.Context(), speech.SynthesizeRequest{Voice: "no-such-voice", Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "available:") {
		t.Errorf("err = %v, want list of available voices", err)
	}
}
