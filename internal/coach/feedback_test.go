package coach

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func split(chunks ...string) (string, []Suggestion) {
	var s Splitter
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(s.Write(c))
	}
	tail, fb := s.Finish()
	b.WriteString(tail)
	return b.String(), fb
}

func TestSplitterSeparatesFeedbackAcrossChunkBoundaries(t *testing.T) {
	reply, fb := split("Ha, lucky you!", " I'd nap.\n\n<feed", "back>\nfix: I has => I have | subject-verb agreement\n",
		"upgrade: \"I am fine.\" => \"I'm good.\"\n</feedback>")
	if reply != "Ha, lucky you! I'd nap." {
		t.Errorf("reply = %q", reply)
	}
	want := []Suggestion{
		{Kind: KindFix, Original: "I has", Suggested: "I have", Reason: "subject-verb agreement"},
		{Kind: KindUpgrade, Original: "I am fine.", Suggested: "I'm good."},
	}
	if diff := cmp.Diff(want, fb); diff != "" {
		t.Errorf("feedback (-want +got):\n%s", diff)
	}
}

func TestSplitterPassesThroughTextWithoutFeedback(t *testing.T) {
	reply, fb := split("if a <", " b then ", "<b>ok</b>  \n")
	if reply != "if a < b then <b>ok</b>" || fb != nil {
		t.Errorf("reply = %q, feedback = %v", reply, fb)
	}
}

func TestSplitterPreservesInnerWhitespace(t *testing.T) {
	if reply, _ := split("Hello", " ", "world", "\n\nNew para."); reply != "Hello world\n\nNew para." {
		t.Errorf("reply = %q", reply)
	}
}

func TestAssessable(t *testing.T) {
	for msg, want := range map[string]bool{
		"x": false, "l": false, "??": false, "1 2": false, "k.": false,
		"Hi": true, "ok": true, "Good.": true, "I has went": true, "Ok byeee.": true,
	} {
		if got := Assessable(msg); got != want {
			t.Errorf("Assessable(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestDropEchoes(t *testing.T) {
	fb := []Suggestion{
		{Kind: KindFix, Original: "x", Suggested: "are you there"},
		{Kind: KindUpgrade, Original: "x", Suggested: "Hey, are you there?"},
		{Kind: KindUpgrade, Original: "Good.", Suggested: "Pretty good, thanks!"},
	}
	got := DropEchoes(fb, "Hey, are you there? I was starting to wonder.")
	if len(got) != 2 || got[1].Suggested != "Pretty good, thanks!" {
		t.Errorf("DropEchoes = %+v", got)
	}
}

func TestCosmeticFixesAreDropped(t *testing.T) {
	cosmeticPairs := [][2]string{
		{"i", "I"}, {"on ig", "on IG"}, {"something. and", "something, and"},
		{"byeee", "bye"}, {"self control", "self-control"}, {"dont", "don't"},
	}
	for _, p := range cosmeticPairs {
		if !cosmetic(p[0], p[1]) {
			t.Errorf("cosmetic(%q, %q) = false, want true", p[0], p[1])
		}
	}
	realPairs := [][2]string{
		{"adress", "address"}, {"I have went", "I went"}, {"to office", "to the office"},
		{"minuite", "minute"}, {"reels trap", "reel trap"}, {"by self control", "through self-control"},
	}
	for _, p := range realPairs {
		if cosmetic(p[0], p[1]) {
			t.Errorf("cosmetic(%q, %q) = true, want false", p[0], p[1])
		}
	}
}

func TestParseFeedbackSkipsMalformedLines(t *testing.T) {
	got := ParseFeedback("note: hi\nfix: no arrow\n- fix: “a” => “b”\nupgrade: same => same\n")
	want := []Suggestion{{Kind: KindFix, Original: "a", Suggested: "b"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
