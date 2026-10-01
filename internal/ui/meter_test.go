package ui

import (
	"math"
	"testing"
)

// heights converts a rendered meter into bar indices so tests can measure how
// much the display moves instead of eyeballing glyphs.
func heights(s string) []int {
	out := make([]int, 0, len([]rune(s)))
	for _, r := range s {
		// IndexRune returns a byte offset, and these glyphs are three bytes each,
		// so count runes rather than trusting the index.
		idx := 0
		for i, br := range meterBars {
			if br == r {
				idx = i
				break
			}
			if i == len(meterBars)-1 {
				panic("unexpected rune " + string(r) + " in meter " + s)
			}
		}
		out = append(out, idx)
	}
	return out
}

// spread is peak-to-trough bar variation across the window: the visual
// "twitchiness" of the meter.
func spread(s string) int {
	h := heights(s)
	if len(h) == 0 {
		return 0
	}
	lo, hi := h[0], h[0]
	for _, v := range h {
		lo, hi = min(lo, v), max(hi, v)
	}
	return hi - lo
}

func avg(v []int) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0
	for _, x := range v {
		s += x
	}
	return float64(s) / float64(len(v))
}

// noisy builds a level window with jitter around a baseline, like a live mic in
// a room with some hum.
func noisy(n int, baseline, jitter float64, seed int64) []float64 {
	out := make([]float64, n)
	s := seed
	for i := range out {
		s = s*6364136223846793005 + 1442695040888963407
		r := float64((s>>33)&0xFFFF)/32768 - 1
		out[i] = math.Max(baseline+r*jitter, 1e-5)
	}
	return out
}

func flat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// A quiet room with background hiss should look almost flat. The previous
// implementation jumped several bars on the same input, which read as noise.
func TestMeterQuietRoomLooksFlat(t *testing.T) {
	m := &meter{}
	levels := noisy(16, 0.02, 0.012, 1)
	for range 40 {
		m.render(levels)
	}
	got := m.render(levels)
	if s := spread(got); s > 1 {
		t.Errorf("quiet room still varies by %d bars: %q", s, got)
	}
	t.Logf("quiet room: %q (spread %d)", got, spread(got))
}

// Speech must stay obvious: this guards against smoothing so far that the meter
// stops being useful as a level indicator.
func TestMeterSpeechStillVisible(t *testing.T) {
	m := &meter{}
	quiet := noisy(16, 0.015, 0.008, 2)
	for range 40 {
		m.render(quiet)
	}
	loud := noisy(16, 0.25, 0.05, 3)
	for range 8 {
		m.render(loud)
	}
	got := m.render(loud)
	t.Logf("speech: %q (avg bar %.2f)", got, avg(heights(got)))
	if avg(heights(got)) < 2 {
		t.Errorf("speech produced an almost flat meter: %q", got)
	}
}

// The contrast between silence and speech is the whole point.
func TestMeterSeparatesSpeechFromSilence(t *testing.T) {
	m := &meter{}
	quiet := noisy(16, 0.015, 0.008, 4)
	for range 40 {
		m.render(quiet)
	}
	quietAvg := avg(heights(m.render(quiet)))

	loud := noisy(16, 0.25, 0.05, 5)
	for range 8 {
		m.render(loud)
	}
	loudAvg := avg(heights(m.render(loud)))

	t.Logf("quiet avg bar %.2f, speech avg bar %.2f", quietAvg, loudAvg)
	if loudAvg-quietAvg < 1.5 {
		t.Errorf("speech is not clearly louder than silence (%.2f vs %.2f)", loudAvg, quietAvg)
	}
}

// Attack must be fast enough that a speech onset is not swallowed.
func TestMeterRespondsQuickly(t *testing.T) {
	m := &meter{}
	quiet := noisy(16, 0.015, 0.008, 6)
	for range 40 {
		m.render(quiet)
	}
	before := avg(heights(m.render(quiet)))

	after := avg(heights(m.render(flat(16, 0.3)))) // one frame after onset
	t.Logf("before onset %.2f -> one frame after %.2f", before, after)
	if after-before < 1 {
		t.Errorf("onset took too long to register (%.2f -> %.2f in one frame)", before, after)
	}
}

// Bars must decay back down, or a single loud moment sticks forever.
func TestMeterDecaysAfterSpeech(t *testing.T) {
	m := &meter{}
	for range 12 {
		m.render(flat(16, 0.3))
	}
	peak := avg(heights(m.render(flat(16, 0.3))))

	for range 80 {
		m.render(flat(16, 0.012))
	}
	settled := avg(heights(m.render(flat(16, 0.012))))
	t.Logf("peak %.2f -> settled %.2f", peak, settled)
	if peak-settled < 1 {
		t.Errorf("bars barely fell after speech stopped (%.2f -> %.2f)", peak, settled)
	}
}

// The window slides one sample at a time; the renderer must not glitch.
func TestMeterHandlesSlidingWindow(t *testing.T) {
	m := &meter{}
	window := flat(16, 0.02)
	for range 20 {
		m.render(window)
	}
	for step := range 50 {
		window = append(window[1:], 0.02+float64(step%3)*0.01)
		got := m.render(window)
		if len([]rune(got)) != 16 {
			t.Fatalf("step %d rendered %d bars, want 16: %q", step, len([]rune(got)), got)
		}
	}
}

func TestMeterHandlesWindowResize(t *testing.T) {
	m := &meter{}
	for _, n := range []int{8, 16, 32, 16} {
		got := m.render(flat(n, 0.1))
		if len([]rune(got)) != n {
			t.Errorf("window %d rendered %d bars", n, len([]rune(got)))
		}
	}
	if got := m.render(nil); got != "" {
		t.Errorf("empty window rendered %q, want empty", got)
	}
}

// Extreme levels must stay inside the bar set: no panic, no wrap, no out-of-range.
func TestMeterClampsExtremeLevels(t *testing.T) {
	for _, v := range []float64{0, 1e-9, 0.5, 1, 2, 100} {
		m := &meter{}
		for range 30 {
			m.render(flat(16, v))
		}
		got := m.render(flat(16, v))
		for _, h := range heights(got) {
			if h < 0 || h >= len(meterBars) {
				t.Fatalf("level %v produced out-of-range bar %d (%q)", v, h, got)
			}
		}
	}
}

// Silence alone must never look like signal.
func TestMeterSilenceRestsAtBottom(t *testing.T) {
	m := &meter{}
	for range 60 {
		m.render(flat(16, 1e-9))
	}
	got := m.render(flat(16, 1e-9))
	t.Logf("silence: %q", got)
	if avg(heights(got)) > 0 {
		t.Errorf("silence should rest at the bottom, got avg bar %.2f: %q", avg(heights(got)), got)
	}
}

// Loud input must read as clearly louder than speech. Full scale is reserved for
// near-clipping levels: a meter that pins at the top on ordinary speech tells
// the user nothing about how loud they actually are.
func TestMeterLoudReadsLouderThanSpeech(t *testing.T) {
	render := func(level float64, frames int) float64 {
		m := &meter{}
		m.render(flat(16, 0.015)) // settle in a quiet room first
		for range 40 {
			m.render(flat(16, 0.015))
		}
		for range frames {
			m.render(flat(16, level))
		}
		return avg(heights(m.render(flat(16, level))))
	}
	speech := render(0.25, 12)
	loud := render(0.8, 12)
	t.Logf("speech avg bar %.2f, loud avg bar %.2f", speech, loud)
	if loud <= speech {
		t.Errorf("loud input (%.2f) not above speech (%.2f)", loud, speech)
	}
	if speech < 2 {
		t.Errorf("ordinary speech reads too low (%.2f): %v", speech, meterBars)
	}
}
