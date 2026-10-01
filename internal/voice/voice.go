// Package voice is the speech I/O layer over the engine stream: it transcribes
// recorded audio and speaks reply text sentence by sentence as it streams in.
package voice

import (
	"context"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/abhijeetadarsh/lumi/internal/audio"
	"github.com/abhijeetadarsh/lumi/internal/provider/speech"
)

// SampleRate is what speech-to-text models are trained on; higher rates only add upload size.
const SampleRate = 16000

func Transcribe(ctx context.Context, t speech.STTTarget, pcm []byte) (speech.Transcript, error) {
	return t.STT.Transcribe(ctx, speech.TranscribeRequest{
		Model:    t.Model,
		Audio:    audio.EncodeWAV(audio.PCM16(SampleRate, 1), pcm),
		Filename: "speech.wav",
		Params:   t.Params,
	})
}

type Speaker struct {
	target speech.TTSTarget
	play   func(ctx context.Context, wav []byte) error
}

func NewSpeaker(t speech.TTSTarget) *Speaker {
	return &Speaker{target: t, play: playWAV}
}

func playWAV(ctx context.Context, wav []byte) error {
	f, pcm, err := audio.DecodeWAV(wav)
	if err != nil {
		return err
	}
	return audio.Play(ctx, f, pcm)
}

// Utterance speaks one reply. Text is synthesized per sentence while earlier
// sentences play, so audio starts before the reply has finished streaming.
type Utterance struct {
	s         *Speaker
	ctx       context.Context
	cancel    context.CancelFunc
	pending   strings.Builder
	sentences chan string
	closed    bool
	done      chan struct{}
	errOnce   sync.Once
	err       error
}

func (s *Speaker) Begin(ctx context.Context) *Utterance {
	ctx, cancel := context.WithCancel(ctx)
	u := &Utterance{s: s, ctx: ctx, cancel: cancel, sentences: make(chan string, 256), done: make(chan struct{})}
	clips := make(chan []byte, 2)
	go u.synthesize(clips)
	go u.playback(clips)
	return u
}

func (u *Utterance) fail(err error) {
	u.errOnce.Do(func() { u.err = err })
	u.cancel()
}

func (u *Utterance) synthesize(clips chan<- []byte) {
	defer close(clips)
	var carry string
	for {
		text := carry
		if text == "" {
			var ok bool
			if text, ok = <-u.sentences; !ok {
				return
			}
		}
		carry = ""
		// Batch sentences that are already waiting: fewer requests, fewer gaps between clips.
	batch:
		for {
			select {
			case next, ok := <-u.sentences:
				if !ok {
					break batch
				}
				if limit := u.s.target.MaxChars; limit > 0 && utf8.RuneCountInString(text)+1+utf8.RuneCountInString(next) > limit {
					carry = next
					break batch
				}
				text += " " + next
			default:
				break batch
			}
		}
		wav, err := u.s.target.TTS.Synthesize(u.ctx, speech.SynthesizeRequest{
			Model:  u.s.target.Model,
			Voice:  u.s.target.Voice,
			Text:   text,
			Params: u.s.target.Params,
		})
		if err != nil {
			u.fail(err)
			return
		}
		select {
		case clips <- wav:
		case <-u.ctx.Done():
			return
		}
	}
}

func (u *Utterance) playback(clips <-chan []byte) {
	defer close(u.done)
	for wav := range clips {
		if err := u.s.play(u.ctx, wav); err != nil {
			u.fail(err)
			return
		}
	}
}

// Write feeds streamed reply text; complete sentences are queued for speech.
func (u *Utterance) Write(text string) {
	u.pending.WriteString(text)
	s := u.pending.String()
	cut := 0
	for i, r := range s {
		if i > 0 && unicode.IsSpace(r) && isSentenceEnd(s[:i]) {
			u.queue(s[cut:i])
			cut = i
		}
	}
	u.pending.Reset()
	u.pending.WriteString(s[cut:])
}

func isSentenceEnd(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	switch r {
	case '.', '!', '?', '…', '\n':
		return true
	}
	return false
}

func (u *Utterance) queue(text string) {
	text = speakable(text)
	if text == "" || u.closed {
		return
	}
	for _, part := range split(text, u.s.target.MaxChars) {
		select {
		case u.sentences <- part:
		case <-u.ctx.Done():
			return
		}
	}
}

// Finish speaks any remaining text and blocks until playback ends.
func (u *Utterance) Finish() error {
	u.queue(u.pending.String())
	u.pending.Reset()
	u.close()
	err := u.ctx.Err()
	u.cancel()
	if u.err != nil {
		return u.err
	}
	return err
}

// Stop silences the utterance immediately.
func (u *Utterance) Stop() {
	u.cancel()
	u.close()
}

func (u *Utterance) close() {
	if !u.closed {
		u.closed = true
		close(u.sentences)
	}
	<-u.done
}

// speakable drops what a voice cannot say: emoji, symbols and markdown markers.
func speakable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case strings.ContainsRune("*_`#~>|", r),
			unicode.Is(unicode.So, r), unicode.Is(unicode.Sk, r), unicode.Is(unicode.Cs, r),
			r == '\u200d', r >= '\ufe00' && r <= '\ufe0f':
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if strings.IndexFunc(out, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0 {
		return ""
	}
	return out
}

// split breaks text into pieces of at most limit runes, preferring commas, then spaces.
func split(text string, limit int) []string {
	var out []string
	for limit > 0 && utf8.RuneCountInString(text) > limit {
		head := string([]rune(text)[:limit])
		cut := strings.LastIndex(head, ", ")
		if cut > 0 {
			cut++
		} else if cut = strings.LastIndexByte(head, ' '); cut <= 0 {
			cut = len(head)
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}
