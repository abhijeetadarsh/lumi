package voice

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/abhijeetadarsh/lumi/internal/audio"
	"github.com/abhijeetadarsh/lumi/internal/provider/openaicompat"
	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

// TestLinuxPushToTalk records from the microphone and runs the real transcription
// path — the same wiring `lumi chat --voice` uses when you press Enter on an empty
// line. Needs a live mic and GROQ_API_KEY.
func TestLinuxPushToTalk(t *testing.T) {
	if os.Getenv("LUMI_TEST_RECORD") == "" {
		t.Skip("set LUMI_TEST_RECORD=1 to record from a real microphone")
	}
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		t.Skip("GROQ_API_KEY not set")
	}

	cl, err := openaicompat.New(openaicompat.Options{BaseURL: "https://api.groq.com/openai/v1", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	stt, ok := any(cl).(speech.STT)
	if !ok {
		t.Fatal("openaicompat client does not implement speech.STT")
	}

	rec, err := audio.Record(audio.PCM16(SampleRate, 1).SampleRate, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("speak now (5s)...")
	time.Sleep(5 * time.Second)
	pcm, err := rec.Stop()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("captured %d bytes (%.2fs)", len(pcm), rec.Format.Duration(pcm).Seconds())
	if len(pcm) == 0 {
		t.Fatal("no audio captured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tr, err := Transcribe(ctx, speech.STTTarget{
		STT:    stt,
		Model:  "whisper-large-v3-turbo",
		Params: map[string]any{"language": "en", "temperature": 0},
	}, pcm)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TRANSCRIPT: %q (lang=%s)", tr.Text, tr.Language)
}
