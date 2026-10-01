package audio

import (
	"os"
	"testing"
	"time"
)

// TestLinuxRecord exercises the Linux capture path against the default microphone.
// It asserts real samples arrive: a silent room still yields a live stream of
// frames. This is the regression test for parecord, which opens a stream and
// reports success but silently never writes any bytes when the server is PipeWire.
func TestLinuxRecord(t *testing.T) {
	if os.Getenv("LUMI_TEST_RECORD") == "" {
		t.Skip("set LUMI_TEST_RECORD=1 to record from a real microphone")
	}
	rec, err := Record(16000, 1)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	pcm, err := rec.Stop() // must return promptly, not deadlock
	if err != nil {
		t.Fatal(err)
	}
	d := rec.Format.Duration(pcm)
	t.Logf("captured %d bytes (%.2fs), level=%.4f", len(pcm), d.Seconds(), rec.Level())
	if len(pcm) == 0 {
		t.Fatal("no audio captured: the recorder opened a stream but produced no samples")
	}
	// 1.2s of 16 kHz 16-bit mono is ~38 kB; require most of it to prove streaming.
	if want := int(1.2 * 16000 * 2 * 0.5); len(pcm) < want {
		t.Errorf("captured %d bytes, below the %d expected for 1.2s of streaming audio", len(pcm), want)
	}
}

// TestLinuxPlay plays a short tone so the sink path is exercised.
func TestLinuxPlay(t *testing.T) {
	if os.Getenv("LUMI_TEST_RECORD") == "" {
		t.Skip("set LUMI_TEST_RECORD=1 to play through a real device")
	}
	f := PCM16(16000, 1)
	pcm := make([]byte, f.bytesPerSecond()/2)
	for i := 0; i+1 < len(pcm); i += 2 {
		pcm[i] = byte(128 + 40)
	}
	if err := Play(t.Context(), f, pcm); err != nil {
		t.Fatal(err)
	}
}
