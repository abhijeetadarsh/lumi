package eval

import (
	"testing"

	"github.com/abhijeetadarsh/lumi/internal/coach"
)

func results(rs []CheckResult) map[string]bool {
	m := make(map[string]bool, len(rs))
	for _, r := range rs {
		m[r.Name] = r.Pass
	}
	return m
}

func TestScore(t *testing.T) {
	ratio := 0.5
	upgrades := 1.0
	checks := Checks{
		MaxQuestionRatio: &ratio,
		MinUpgradeRatio:  &upgrades,
		MaxReplyWords:    6,
		BannedPhrases:    []string{"that's great to hear"},
		OncePhrases:      []string{"sounds like"},
		NoFixOn:          []int{0},
		ExpectFix:        []Expect{{Turn: 1, Original: "have went", Suggested: "went"}},
	}

	good := []TurnResult{
		{Input: "hey", Reply: "Hey! Rainy day here.", Feedback: []coach.Suggestion{{Kind: coach.KindUpgrade, Original: "hey", Suggested: "Hey there!"}}},
		{Input: "I have went", Reply: "Where to?", Feedback: []coach.Suggestion{
			{Kind: coach.KindFix, Original: "I have went", Suggested: "I went"},
			{Kind: coach.KindUpgrade, Original: "I have went", Suggested: "I went out"},
		}},
	}
	for name, pass := range results(Score(checks, good)) {
		if !pass {
			t.Errorf("good transcript failed %s", name)
		}
	}

	bad := []TurnResult{
		{Input: "hey", Reply: "That’s great to hear! Sounds like fun. How are you?", Feedback: []coach.Suggestion{{Kind: coach.KindFix, Original: "hey", Suggested: "hello"}}},
		{Input: "I have went", Reply: "Nice. <feedback> fix: a => b and what else is going on today? Sounds like a plan?"},
		{Input: "I have went", Reply: `Oh, the "have went" combo.`, Feedback: []coach.Suggestion{{Kind: coach.KindFix, Original: "I have went", Suggested: "I went"}}},
	}
	got := results(Score(checks, bad))
	for _, name := range []string{"clean_reply", "question_ratio", "upgrade_ratio", "reply_length", "banned_phrases", "no_repeated_phrases", "reply_ignores_mistakes", "no_false_fixes", "expect_fix#0"} {
		if got[name] {
			t.Errorf("bad transcript passed %s", name)
		}
	}
}
