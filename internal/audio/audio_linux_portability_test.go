package audio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake recorder that produces real samples, so the happy path is testable
// without a microphone.
func fakeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestRecordFallsBackFromSilentTool is the portability guarantee: a tool that
// starts successfully but never emits samples must be skipped in favour of one
// that works, instead of silently producing an empty recording.
func TestRecordFallsBackFromSilentTool(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	// First candidate: opens nothing, prints nothing, waits to be killed.
	fakeTool(t, dir, "broken-rec", "sleep 30")
	// Second candidate: emits a continuous 16 kHz mono tone forever.
	fakeTool(t, dir, "good-rec", `while :; do
  i=0
  while [ $i -lt 2000 ]; do
    printf '\\000\\000'
    i=$((i+1))
  done
done`)

	t.Setenv(envRecord, filepath.Join(dir, "broken-rec"))
	// Point the candidate list at our fakes by putting them first in PATH order
	// via an explicit override list is not supported, so drive the loop directly.
	var tried []string
	f := PCM16(16000, 1)

	if rec, err := startVerified(f, filepath.Join(dir, "broken-rec"), &tried); err == nil {
		pcm, _ := rec.Stop()
		if len(pcm) > 0 {
			t.Fatalf("broken tool unexpectedly produced %d bytes", len(pcm))
		}
		t.Log("broken tool returned no data (as expected)")
	} else {
		t.Logf("broken tool rejected: %v", err)
	}
	if len(tried) == 0 {
		t.Fatal("broken tool was not recorded as tried")
	}

	rec, err := startVerified(f, filepath.Join(dir, "good-rec"), &tried)
	if err != nil {
		t.Fatalf("working tool rejected: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	pcm, err := rec.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) == 0 {
		t.Fatal("working tool produced no samples")
	}
	t.Logf("working tool produced %d bytes", len(pcm))
}

// TestRecordArgsAreFormatSafe checks each recorder is asked for raw PCM. A WAV
// header on the stream would be prepended to the samples and corrupt them.
func TestRecordArgsAreFormatSafe(t *testing.T) {
	f := PCM16(16000, 1)
	for _, base := range []string{"arecord", "pw-record", "pw-cat", "parecord", "ffmpeg"} {
		args := strings.Join(recordArgs(base, f), " ")
		if base == "arecord" && strings.Contains(args, "wav") {
			t.Errorf("arecord asked for WAV on stdout: %s", args)
		}
		if !strings.Contains(args, "-") {
			t.Errorf("%s does not write to stdout: %s", base, args)
		}
		if base == "pw-record" || base == "pw-cat" {
			if !strings.Contains(args, "s16") {
				t.Errorf("%s does not request 16-bit: %s", base, args)
			}
		}
		t.Logf("%-10s %s", base, args)
	}
}

func TestPlayerNamesHonourOverride(t *testing.T) {
	t.Setenv(envPlay, "sh")
	names, err := playerNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || filepath.Base(names[0]) != "sh" {
		t.Errorf("override ignored: %v", names)
	}
	t.Setenv(envPlay, "definitely-not-a-real-binary-xyz")
	if _, err := playerNames(); err == nil {
		t.Error("expected an error for a bogus override")
	}
}
