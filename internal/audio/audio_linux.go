//go:build linux

package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Linux goes through the PulseAudio/PipeWire command-line tools rather than a
// cgo binding, which keeps the "no C toolchain" property of the Windows build.
// LUMI_AUDIO_RECORD / LUMI_AUDIO_PLAY override the executables; LUMI_AUDIO_RECORD_ARGS
// and LUMI_AUDIO_PLAY_ARGS append extra flags for unusual setups.
const (
	envRecord     = "LUMI_AUDIO_RECORD"
	envPlay       = "LUMI_AUDIO_PLAY"
	envRecordArgs = "LUMI_AUDIO_RECORD_ARGS"
	envPlayArgs   = "LUMI_AUDIO_PLAY_ARGS"
)

// Playback tools, most preferred first. paplay and pw-play read a WAV from
// stdin; aplay and play (sox) read raw samples and need the format flags.
var playerCandidates = []string{
	"paplay", "pw-play", "aplay", "play", "ffplay", "cvlc", "mpv", "gst-play-1.0",
}

// Recorders, most preferred first.
//
// The native PipeWire tools (pw-record, pw-cat) come first because the
// PulseAudio clients are unreliable against a PipeWire server: parecord opens a
// stream, reports "Stream successfully created" with Corked: no, shows up in
// `pactl list source-outputs`, and then never writes a single byte to stdout.
// ffmpeg is next as a widely packaged, format-explicit fallback, then plain ALSA
// (arecord) for systems with no sound server at all.
var recorderCandidates = []string{
	"pw-record", "pw-cat", "parecord", "ffmpeg", "arecord", "rec",
}

// playerFor returns the command that plays a WAV stream on stdin. Most tools
// read a complete WAV; aplay, play and pw-play read raw samples and so are
// given explicit format flags instead.
func playerFor(name string, f Format) (*exec.Cmd, error) {
	base := filepath.Base(name)
	var args []string
	switch base {
	case "aplay":
		args = []string{"-q", "-t", "wav", "-f", aplayFormat(f), "-"}
	case "play":
		args = []string{"-q", "-t", "wav", "-"}
	case "pw-play":
		args = []string{"--format", pwFormat(f), "--rate", strconv.Itoa(f.SampleRate),
			"--channels", strconv.Itoa(f.Channels), "-"}
	case "gst-play-1.0":
		args = []string{"fdsrc", "!", "wavparse", "!", "autoaudiosink"}
	case "cvlc":
		args = []string{"--play-and-exit", "--intf", "dummy", "pipe:", "0"}
	case "mpv":
		args = []string{"--no-video", "--really-quiet", "pipe:0"}
	default:
		// paplay, ffplay and anything else read a complete WAV off stdin.
	}
	args = append(args, strings.Fields(strings.TrimSpace(os.Getenv(envPlayArgs)))...)
	return exec.Command(name, args...), nil
}

// playerNames lists the installed playback tools in preference order, honouring
// the LUMI_AUDIO_PLAY override.
func playerNames() ([]string, error) {
	if custom := strings.TrimSpace(os.Getenv(envPlay)); custom != "" {
		abs, err := exec.LookPath(custom)
		if err != nil {
			return nil, fmt.Errorf("audio: %s=%q not found in PATH", envPlay, custom)
		}
		return []string{abs}, nil
	}
	var names []string
	for _, c := range playerCandidates {
		if p, err := exec.LookPath(c); err == nil {
			names = append(names, p)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("audio: no player found (tried %v); install pipewire-utils, alsa-utils or ffmpeg, or set %s",
			playerCandidates, envPlay)
	}
	return names, nil
}

// Play plays PCM on the default output device, blocking until it finishes or
// ctx is done. If a player is installed but cannot open the device (a stale
// sound server, a busy device), the next candidate is tried rather than failing
// the whole turn.
func Play(ctx context.Context, f Format, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	names, err := playerNames()
	if err != nil {
		return err
	}
	var lastErr error
	for _, name := range names {
		if err := playOnce(ctx, name, f, pcm); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("audio: no player could play the audio: %w", lastErr)
}

func playOnce(ctx context.Context, name string, f Format, pcm []byte) error {
	cmd, err := playerFor(name, f)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	cmd.Stdin = bytes.NewReader(EncodeWAV(f, pcm))
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(name), err)
	}

	// Killing the player is what stops playback promptly on cancel.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, strings.TrimSpace(out.String()))
		}
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		return ctx.Err()
	}
}

// recordArgs builds the argument list for a given recorder.
func recordArgs(base string, f Format) []string {
	sampleRate, channels := strconv.Itoa(f.SampleRate), strconv.Itoa(f.Channels)
	switch base {
	case "arecord", "rec":
		// ALSA writes a WAV header with -t wav, which would corrupt the raw
		// stream; -t raw gives exactly the PCM the caller expects.
		return []string{"-q", "-t", "raw", "-f", aplayFormat(f), "-r", sampleRate, "-c", channels, "-"}
	case "pw-record":
		return []string{"--format", pwFormat(f), "--rate", sampleRate, "--channels", channels, "-"}
	case "pw-cat":
		return []string{"-r", "--format", pwFormat(f), "--rate", sampleRate, "--channels", channels, "-"}
	case "ffmpeg":
		// -f alsa goes straight to the device, which also covers systems whose
		// sound server is not running.
		return []string{"-hide_banner", "-loglevel", "error", "-f", "alsa",
			"-ac", channels, "-ar", sampleRate, "-i", "default", "-f", "s16le", "-"}
	default: // parecord
		return []string{"--format", "s16le", "--rate", sampleRate, "--channels", channels, "-"}
	}
}

// startRecorder launches one candidate and returns the running command plus a
// reader for its stdout.
func startRecorder(name string, f Format) (*exec.Cmd, io.ReadCloser, *bytes.Buffer, error) {
	base := filepath.Base(name)
	args := append(recordArgs(base, f), strings.Fields(strings.TrimSpace(os.Getenv(envRecordArgs)))...)
	cmd := exec.Command(name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, &errBuf, nil
}

// probeTimeout is how long we wait for a recorder's first samples before
// deciding it is not actually working.
const probeTimeout = 1500 * time.Millisecond

// Record starts capturing 16-bit PCM from the default microphone. It records
// until Stop is called; levels update as audio arrives so the UI meter can
// animate.
//
// Candidates are tried in order and each is verified: a tool that starts but
// never emits samples (parecord against a PipeWire server) is abandoned and the
// next one is tried, because that failure is otherwise silent.
func Record(sampleRate, channels int) (*Recording, error) {
	f := PCM16(sampleRate, channels)

	var names []string
	if custom := strings.TrimSpace(os.Getenv(envRecord)); custom != "" {
		abs, err := exec.LookPath(custom)
		if err != nil {
			return nil, fmt.Errorf("audio: %s=%q not found in PATH", envRecord, custom)
		}
		names = []string{abs}
	} else {
		for _, c := range recorderCandidates {
			if p, err := exec.LookPath(c); err == nil {
				names = append(names, p)
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("audio: no recorder found (tried %v); install pipewire-utils, alsa-utils or ffmpeg, or set %s",
			recorderCandidates, envRecord)
	}

	var tried []string
	var lastErr error
	for _, name := range names {
		rec, err := startVerified(f, name, &tried)
		if err == nil {
			return rec, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("audio: every recorder failed to capture audio (%s): %w",
		strings.Join(tried, ", "), lastErr)
}

// startVerified launches a recorder and waits briefly for real samples. If the
// process dies or stays silent, it is killed and the error is returned so the
// caller can try the next candidate.
func startVerified(f Format, name string, tried *[]string) (*Recording, error) {
	base := filepath.Base(name)
	cmd, stdout, errBuf, err := startRecorder(name, f)
	if err != nil {
		return nil, fmt.Errorf("audio: start %s: %w", base, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Peek at the first chunk with a deadline; anything already buffered is
	// kept so no audio is lost.
	buf := make([]byte, 4096)
	type readRes struct {
		n   int
		err error
	}
	first := make(chan readRes, 1)
	go func() {
		n, err := stdout.Read(buf)
		first <- readRes{n, err}
	}()

	select {
	case r := <-first:
		if r.n > 0 {
			return newRecordingFrom(cmd, stdout, buf[:r.n], f, base, errBuf, exited), nil
		}
		*tried = append(*tried, base+": no data")
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("audio: %s produced no samples: %w", base, r.err)
	case err := <-exited:
		*tried = append(*tried, base+": exited")
		return nil, fmt.Errorf("audio: %s exited immediately (%v): %s", base, err, errBuf.String())
	case <-time.After(probeTimeout):
		// No samples yet. A real device can be slow to start, so only give up
		// once the process has also produced nothing to show for it.
		select {
		case r := <-first:
			if r.n > 0 {
				return newRecordingFrom(cmd, stdout, buf[:r.n], f, base, errBuf, exited), nil
			}
		case <-time.After(100 * time.Millisecond):
		}
		*tried = append(*tried, base+": silent")
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("audio: %s produced no samples within %v", base, probeTimeout)
	}
}

// newRecordingFrom starts consuming a verified recorder's output, seeded with
// any samples already read during verification.
func newRecordingFrom(cmd *exec.Cmd, stdout io.ReadCloser, seed []byte, f Format, base string, errBuf *bytes.Buffer, exited chan error) *Recording {
	rec := newRecording(f)
	go func() {
		defer close(rec.done)
		// Stop() closes rec.stop; end the tool so stdout reaches EOF and the
		// read loop can drain what is still buffered. SIGINT asks for a clean
		// exit that flushes; SIGKILL is the fallback if it ignores us.
		// Wait must not run before stdout is drained, since it closes the pipe.
		go func() { <-exited }()
		go func() {
			<-rec.stop
			if cmd.Process != nil {
				_ = cmd.Process.Signal(os.Interrupt)
				time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
			}
		}()

		if n := len(seed); n > 0 {
			rec.pcm = append(rec.pcm, seed[:n]...)
			rec.lvl.Store(math.Float64bits(level(seed[:n])))
		}

		frame := make([]byte, f.blockAlign()*160) // 10 ms at any rate
		for {
			n, err := stdout.Read(frame)
			if n > 0 {
				chunk := frame[:n]
				rec.pcm = append(rec.pcm, chunk...)
				rec.lvl.Store(math.Float64bits(level(chunk)))
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) && rec.err == nil {
					rec.err = fmt.Errorf("audio: %s: %w: %s", base, err, errBuf.String())
				}
				return
			}
		}
	}()
	return rec
}

func aplayFormat(f Format) string {
	tag := "S16_LE"
	if f.Tag == formatFloat {
		tag = "FLOAT_LE"
	}
	return fmt.Sprintf("%s,%d,%d,%d", tag, f.Channels, f.SampleRate, f.blockAlign())
}

func pwFormat(f Format) string {
	if f.Tag == formatFloat {
		return "f32"
	}
	return "s16"
}
