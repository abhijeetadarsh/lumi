// Package ui renders the chat in the terminal: styled and word-wrapped on a TTY,
// plain text when output is piped or NO_COLOR is set.
package ui

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"github.com/abhijeetadarsh/lumi/internal/coach"
	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
)

const (
	maxWidth = 100
	nameW    = 6             // speaker names are right-aligned in this column
	gutterW  = nameW + 3     // "  lumi │ "
	fbW      = nameW + 1 + 7 // "      ├ fix  "
	gold     = "#F6C177"
	violet   = "#C4A7E7"
)

var pad = strings.Repeat(" ", nameW+1)

type styles struct {
	lumi, you, rule, dim, bold, fixOld, fixNew, say, err, warn lipgloss.Style
}

type Renderer struct {
	// Voice switches the prompt and hints to push-to-talk mode.
	Voice bool
	out   io.Writer
	tty   bool
	width int
	lg    *lipgloss.Renderer
	st    styles
}

func New(f *os.File) *Renderer {
	r := &Renderer{out: f, width: 80, lg: lipgloss.NewRenderer(f)}
	r.tty = term.IsTerminal(int(f.Fd())) && r.lg.ColorProfile() != termenv.Ascii
	if r.tty {
		_, _ = termenv.EnableVirtualTerminalProcessing(r.lg.Output())
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 20 {
			r.width = min(w, maxWidth)
		}
	}
	c := func(hex string) lipgloss.Style { return r.lg.NewStyle().Foreground(lipgloss.Color(hex)) }
	r.st = styles{
		lumi:   c(gold).Bold(true),
		you:    c("#9CCFD8").Bold(true),
		rule:   c("#6E6A86"),
		dim:    c("#908CAA"),
		bold:   r.lg.NewStyle().Bold(true),
		fixOld: c("#EB6F92").Strikethrough(true),
		fixNew: c("#A6DA95"),
		say:    c("#9CCFD8"),
		err:    c("#EB6F92"),
		warn:   c("#F5A97F"),
	}
	return r
}

// gutter renders the speaker column, e.g. "  lumi │ ".
func (r *Renderer) gutter(name string, st lipgloss.Style, sep string) string {
	return st.Render(fmt.Sprintf("%*s", nameW, name)) + " " + r.st.rule.Render(sep) + " "
}

// branch renders an empty speaker column followed by a rule glyph.
func (r *Renderer) branch(glyph string) string { return pad + r.st.rule.Render(glyph) }

func (r *Renderer) gradient(s string) string {
	from, to := hexRGB(gold), hexRGB(violet)
	runes := []rune(s)
	var b strings.Builder
	for i, c := range runes {
		f := float64(i) / float64(max(len(runes)-1, 1))
		hex := fmt.Sprintf("#%02X%02X%02X",
			int(from[0]+(to[0]-from[0])*f), int(from[1]+(to[1]-from[1])*f), int(from[2]+(to[2]-from[2])*f))
		b.WriteString(r.lg.NewStyle().Foreground(lipgloss.Color(hex)).Render(string(c)))
	}
	return b.String()
}

func hexRGB(hex string) [3]float64 {
	var rgb [3]int
	fmt.Sscanf(hex, "#%02X%02X%02X", &rgb[0], &rgb[1], &rgb[2])
	return [3]float64{float64(rgb[0]), float64(rgb[1]), float64(rgb[2])}
}

func (r *Renderer) Banner(module string, version int, sessionID string) {
	if !r.tty {
		fmt.Fprintf(r.out, "lumi | %s v%d | session %s\n", module, version, sessionID)
		return
	}
	st := r.st
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, "  "+st.lumi.Render("◑ lumi")+"  "+st.bold.Render(module)+st.dim.Render(fmt.Sprintf(" v%d", version)))
	fmt.Fprintln(r.out, "  "+r.gradient(strings.Repeat("━", min(r.width-4, 56))))
	fmt.Fprintln(r.out, "  "+st.dim.Render("session ")+st.rule.Render(sessionID))
	hint := "/exit to leave · ctrl+c stops a reply"
	if r.Voice {
		hint = "enter on an empty line to talk · " + hint
	}
	fmt.Fprintln(r.out, "  "+st.dim.Render(hint))
	fmt.Fprintln(r.out)
}

func (r *Renderer) Prompt() string {
	if !r.tty {
		return "> "
	}
	if r.Voice {
		return r.gutter("you", r.st.you, "∿")
	}
	return r.gutter("you", r.st.you, "›")
}

var meterBars = []rune("▁▂▃▄▅▆▇█")

// RecordingPrompt shows a live mic meter; levels are recent loudness samples in [0, 1].
func (r *Renderer) RecordingPrompt(elapsed time.Duration, levels []float64) string {
	if !r.tty {
		return fmt.Sprintf("recording %ds, enter to send> ", int(elapsed.Seconds()))
	}
	var meter strings.Builder
	for _, l := range levels {
		// Map -50..-10 dBFS onto the bar heights; speech sits roughly in that range.
		db := 20 * math.Log10(max(l, 1e-6))
		i := int((db + 50) / 40 * float64(len(meterBars)-1))
		meter.WriteRune(meterBars[min(max(i, 0), len(meterBars)-1)])
	}
	dot := " "
	if elapsed%time.Second < 700*time.Millisecond {
		dot = "●"
	}
	secs := int(elapsed.Seconds())
	return r.st.you.Render(fmt.Sprintf("%*s", nameW, "you")) + " " + r.st.err.Render(dot) + " " +
		r.st.dim.Render(fmt.Sprintf("%d:%02d ", secs/60, secs%60)) + r.st.lumi.Render(meter.String()) +
		r.st.rule.Render("  enter send · ctrl+c cancel ")
}

// Transcribing replaces the recording line (the line above the cursor) with a status.
func (r *Renderer) Transcribing() {
	if r.tty {
		io.WriteString(r.out, "\x1b[1A\r\x1b[K"+r.gutter("you", r.st.you, "∿")+r.st.dim.Render("transcribing…"))
	}
}

// Heard replaces the transcribing status with what was understood.
func (r *Renderer) Heard(text string) {
	if !r.tty {
		fmt.Fprintln(r.out, "you (voice):", text)
		return
	}
	if text == "" {
		text = r.st.dim.Render("didn't catch that, try again")
	}
	fmt.Fprintln(r.out, "\r\x1b[K"+r.gutter("you", r.st.you, "∿")+text)
}

// RecordingCancelled replaces the recording line with a note.
func (r *Renderer) RecordingCancelled(note string) {
	if !r.tty {
		fmt.Fprintln(r.out, note)
		return
	}
	fmt.Fprintln(r.out, "\x1b[1A\r\x1b[K"+r.gutter("you", r.st.you, "∿")+r.st.dim.Render(note))
}

func (r *Renderer) Error(err error) {
	if !r.tty {
		fmt.Fprintln(r.out, "error:", err)
		return
	}
	fmt.Fprintln(r.out, r.gutter("error", r.st.err, "│")+r.st.err.Render(err.Error()))
	fmt.Fprintln(r.out)
}

// Turn renders one streamed assistant turn.
type Turn struct {
	r        *Renderer
	spin     *spinner
	started  bool
	ended    bool
	col      int
	newlines int // pending line breaks, written only once more text follows
	word     strings.Builder
}

func (r *Renderer) BeginTurn() *Turn {
	t := &Turn{r: r}
	if r.tty {
		t.spin = startSpinner(r)
	}
	return t
}

func (t *Turn) Text(s string) {
	t.stopSpinner()
	out := t.r.out
	if !t.started {
		if s = strings.TrimLeftFunc(s, unicode.IsSpace); s == "" {
			return
		}
		t.started = true
		if t.r.tty {
			io.WriteString(out, t.r.gutter("lumi", t.r.st.lumi, "│"))
			t.col = gutterW
		}
	}
	if !t.r.tty {
		io.WriteString(out, s)
		return
	}
	for _, c := range s {
		switch {
		case c == '\n':
			t.flushWord()
			t.newlines++
		case unicode.IsSpace(c):
			t.flushWord()
			if t.newlines == 0 && t.col > gutterW && t.col < t.r.width {
				io.WriteString(out, " ")
				t.col++
			}
		default:
			t.word.WriteRune(c)
		}
	}
}

func (t *Turn) flushWord() {
	if t.word.Len() == 0 {
		return
	}
	w := t.word.String()
	t.word.Reset()
	n := ansi.StringWidth(w)
	out, cont := t.r.out, t.r.branch("│")
	switch {
	case t.newlines > 0:
		io.WriteString(out, "\n")
		if t.newlines > 1 {
			io.WriteString(out, cont+"\n")
		}
		io.WriteString(out, cont+" ")
		t.col, t.newlines = gutterW, 0
	case t.col+n > t.r.width && t.col > gutterW:
		io.WriteString(out, "\n"+cont+" ")
		t.col = gutterW
	}
	io.WriteString(out, w)
	t.col += n
}

func (t *Turn) endText() {
	t.stopSpinner()
	if t.ended {
		return
	}
	t.ended = true
	if t.r.tty {
		t.flushWord()
	}
	if t.started {
		io.WriteString(t.r.out, "\n")
	}
}

func (t *Turn) Feedback(items []coach.Suggestion) {
	t.endText()
	st, out := t.r.st, t.r.out
	if t.r.tty && len(items) > 0 {
		fmt.Fprintln(out, t.r.branch("│"))
	}
	for i, s := range items {
		if !t.r.tty {
			fmt.Fprintf(out, "%s: %s => %s", s.Kind, s.Original, s.Suggested)
			if s.Reason != "" {
				fmt.Fprintf(out, " (%s)", s.Reason)
			}
			fmt.Fprintln(out)
			continue
		}
		var label, body string
		switch s.Kind {
		case coach.KindFix:
			label = st.err.Render("fix")
			body = st.fixOld.Render(s.Original) + st.dim.Render(" → ") + st.fixNew.Render(s.Suggested)
			if s.Reason != "" {
				body += st.dim.Render("  " + s.Reason)
			}
		case coach.KindUpgrade:
			label = st.say.Render("say")
			body = st.say.Render(s.Suggested)
		}
		prefix, cont := t.r.branch("├")+" "+label+"  ", t.r.branch("│")+"      "
		if i == len(items)-1 {
			prefix, cont = t.r.branch("╰")+" "+label+"  ", strings.Repeat(" ", fbW)
		}
		for j, l := range strings.Split(ansi.Wrap(body, t.r.width-fbW, ""), "\n") {
			if j > 0 {
				prefix = cont
			}
			fmt.Fprintln(out, prefix+l)
		}
	}
}

func (t *Turn) Done(latency time.Duration, usage *llm.Usage) {
	t.endText()
	if !t.r.tty {
		return
	}
	meta := fmt.Sprintf("%.1fs", latency.Seconds())
	if usage != nil {
		meta += fmt.Sprintf(" · %d in · %d out", usage.InputTokens, usage.OutputTokens)
	}
	fmt.Fprintln(t.r.out, strings.Repeat(" ", max(t.r.width-len([]rune(meta)), gutterW))+t.r.st.rule.Render(meta))
	fmt.Fprintln(t.r.out)
}

func (t *Turn) Interrupted() {
	t.endText()
	if !t.r.tty {
		fmt.Fprintln(t.r.out, "[interrupted]")
		return
	}
	fmt.Fprintln(t.r.out, t.r.branch("╰")+" "+t.r.st.warn.Render("interrupted"))
	fmt.Fprintln(t.r.out)
}

// Abort ends the turn's output cleanly before an error is reported.
func (t *Turn) Abort() { t.endText() }

func (t *Turn) stopSpinner() {
	if t.spin != nil {
		t.spin.stop()
		t.spin = nil
	}
}

var spinFrames = []string{"○", "◔", "◑", "◕", "●", "◕", "◑", "◔"}

type spinner struct {
	stopCh chan struct{}
	done   chan struct{}
	once   sync.Once
}

func startSpinner(r *Renderer) *spinner {
	s := &spinner{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		start := time.Now()
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		io.WriteString(r.out, "\x1b[?25l")
		for i := 0; ; i++ {
			line := r.gutter("lumi", r.st.lumi, "│") + r.st.lumi.Render(spinFrames[i%len(spinFrames)]) +
				r.st.dim.Render(fmt.Sprintf(" thinking · %ds", int(time.Since(start).Seconds())))
			io.WriteString(r.out, "\r"+line+"\x1b[K")
			select {
			case <-s.stopCh:
				io.WriteString(r.out, "\r\x1b[K\x1b[?25h")
				return
			case <-tick.C:
			}
		}
	}()
	return s
}

func (s *spinner) stop() {
	s.once.Do(func() { close(s.stopCh) })
	<-s.done
}
