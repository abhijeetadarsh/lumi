package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/ergochat/readline"
	"github.com/spf13/cobra"

	"github.com/abhijeetadarsh/lumi/internal/app"
	"github.com/abhijeetadarsh/lumi/internal/audio"
	"github.com/abhijeetadarsh/lumi/internal/engine"
	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
	"github.com/abhijeetadarsh/lumi/internal/store"
	"github.com/abhijeetadarsh/lumi/internal/ui"
	"github.com/abhijeetadarsh/lumi/internal/voice"
)

var errCancelled = errors.New("cancelled")

// minRecording filters accidental double-Enter taps.
const minRecording = 400 * time.Millisecond

type voiceIO struct {
	stt     *speech.STTTarget
	speaker *voice.Speaker
}

func newChatCmd() *cobra.Command {
	var (
		moduleName, sessionID string
		voiceMode             bool
	)
	cmd := &cobra.Command{
		Use:   "chat [message...]",
		Short: "Chat with Lumi (interactive unless a message is given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			a, err := app.Open(ctx)
			if err != nil {
				return err
			}
			defer a.Close()

			var vio voiceIO
			if voiceMode {
				stt, tts, err := a.Voice()
				if err != nil {
					return err
				}
				if stt == nil && tts == nil {
					return errors.New("voice mode needs voice.stt and/or voice.tts in the config (see `lumi config init`)")
				}
				vio.stt = stt
				if tts != nil {
					vio.speaker = voice.NewSpeaker(*tts)
				}
			}

			sess, err := openSession(ctx, a, moduleName, sessionID)
			if err != nil {
				return err
			}
			defer func() {
				// WithoutCancel: the user may have hit Ctrl+C, but we still want to
				// close the session and learn from it.
				bg := context.WithoutCancel(ctx)
				if err := a.Engine.EndSession(bg, sess.ID); err != nil {
					slog.Warn("end session", "err", err)
				}
				reflectMem(bg, a, sess.ID)
			}()

			r := ui.New(os.Stdout)
			r.Voice = vio.stt != nil
			if len(args) > 0 {
				return runTurn(ctx, a.Engine, sess.ID, engine.Input{Text: strings.Join(args, " ")}, r, vio.speaker)
			}
			return repl(ctx, a, sess, r, vio)
		},
	}
	cmd.Flags().StringVarP(&moduleName, "module", "m", "", "module to use (default: config default_module)")
	cmd.Flags().StringVarP(&sessionID, "session", "s", "", "resume a session by ID")
	cmd.Flags().BoolVar(&voiceMode, "voice", false, "talk with push-to-talk and hear replies (uses the voice config)")
	cmd.MarkFlagsMutuallyExclusive("module", "session")
	return cmd
}

func openSession(ctx context.Context, a *app.App, moduleName, sessionID string) (store.Session, error) {
	if sessionID != "" {
		return a.Engine.Session(ctx, sessionID)
	}
	if moduleName == "" {
		moduleName = a.Config.DefaultModule
	}
	return a.Engine.StartSession(ctx, moduleName)
}

// runTurn streams one reply, speaking it when spk is set; Ctrl+C interrupts the reply
// (or its speech) rather than the program.
func runTurn(parent context.Context, eng *engine.Engine, sessionID string, in engine.Input, r *ui.Renderer, spk *voice.Speaker) error {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()

	var utt *voice.Utterance
	if spk != nil {
		in.Spoken = true
		utt = spk.Begin(ctx)
	}
	t := r.BeginTurn()
	for ev, err := range eng.Turn(ctx, sessionID, in) {
		if err != nil {
			if utt != nil {
				utt.Stop()
			}
			if ctx.Err() != nil && parent.Err() == nil {
				t.Interrupted()
				return errCancelled
			}
			t.Abort()
			return err
		}
		switch ev.Kind {
		case engine.EventText:
			t.Text(ev.Text)
			if utt != nil {
				utt.Write(ev.Text)
			}
		case engine.EventFeedback:
			t.Feedback(ev.Feedback)
		case engine.EventDone:
			t.Done(ev.Latency, ev.Usage)
		}
	}
	if utt != nil {
		if err := utt.Finish(); err != nil && ctx.Err() == nil {
			return fmt.Errorf("speak: %w", err)
		}
	}
	return nil
}

func repl(ctx context.Context, a *app.App, sess store.Session, r *ui.Renderer, vio voiceIO) error {
	r.Banner(sess.Module, sess.ModuleVersion, sess.ID)
	rl, err := readline.NewFromConfig(&readline.Config{
		Prompt:          r.Prompt(),
		HistoryFile:     filepath.Join(a.Config.Home, "history"),
		InterruptPrompt: "^C",
	})
	if err != nil {
		return fmt.Errorf("init readline: %w", err)
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			continue
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}

		in := engine.Input{Text: strings.TrimSpace(line)}
		switch in.Text {
		case "":
			if vio.stt == nil {
				continue
			}
			text, err := listen(ctx, rl, r, *vio.stt)
			if err != nil {
				r.Error(err)
			}
			if text == "" {
				continue
			}
			in.Text, in.Transcribed = text, true
		case "/exit", "/quit":
			return nil
		}

		if err := runTurn(ctx, a.Engine, sess.ID, in, r, vio.speaker); err != nil && !errors.Is(err, errCancelled) {
			r.Error(err)
		}
	}
}

// reflectMem distils the finished session into durable facts. It runs after the
// session ends and never blocks the exit for long: a failed or slow reflection is
// logged and forgotten, since memory is an optimisation, not a correctness need.
func reflectMem(ctx context.Context, a *app.App, sessionID string) {
	m := a.Memory()
	if m == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	n, err := m.Reflect(ctx, sessionID)
	switch {
	case err != nil:
		slog.Warn("reflect session", "session", sessionID, "err", err)
	case n > 0:
		slog.Debug("reflected session", "session", sessionID, "facts", n)
	}
}

// listen records until Enter (Ctrl+C cancels) and returns the transcript; "" means nothing usable was said.
func listen(ctx context.Context, rl *readline.Instance, r *ui.Renderer, stt speech.STTTarget) (string, error) {
	rec, err := audio.Record(voice.SampleRate, 1)
	if err != nil {
		return "", err
	}

	levels := make([]float64, 16)
	rl.SetPrompt(r.RecordingPrompt(0, levels))
	stopMeter, meterDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(meterDone)
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopMeter:
				return
			case <-tick.C:
				levels = append(levels[1:], rec.Level())
				rl.SetPrompt(r.RecordingPrompt(rec.Elapsed(), levels))
				rl.Refresh()
			}
		}
	}()

	rl.DisableHistory()
	_, readErr := rl.Readline()
	rl.EnableHistory()
	close(stopMeter)
	<-meterDone
	rl.SetPrompt(r.Prompt())

	pcm, err := rec.Stop()
	switch {
	case readErr != nil:
		r.RecordingCancelled("cancelled")
		return "", nil
	case err != nil:
		return "", err
	case rec.Format.Duration(pcm) < minRecording:
		r.RecordingCancelled("too short, hold on a bit longer")
		return "", nil
	}

	r.Transcribing()
	sctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	tr, err := voice.Transcribe(sctx, stt, pcm)
	if err != nil {
		r.Heard("")
		if sctx.Err() != nil && ctx.Err() == nil {
			return "", nil
		}
		return "", fmt.Errorf("transcribe: %w", err)
	}
	r.Heard(tr.Text)
	return tr.Text, nil
}
