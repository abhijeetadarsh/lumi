// Package eval replays scripted conversations against a module and scores the
// transcript with deterministic checks, so prompt changes are measured, not guessed.
package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/abhijeetadarsh/lumi/internal/coach"
	"github.com/abhijeetadarsh/lumi/internal/engine"
)

type Suite struct {
	Module string `yaml:"module"`
	Cases  []Case `yaml:"cases"`
}

type Case struct {
	Name   string   `yaml:"name"`
	Turns  []string `yaml:"turns"`
	Checks Checks   `yaml:"checks"`
}

type Checks struct {
	MaxQuestionRatio *float64 `yaml:"max_question_ratio"`
	MinUpgradeRatio  *float64 `yaml:"min_upgrade_ratio"`
	MaxReplyWords    int      `yaml:"max_reply_words"`
	BannedPhrases    []string `yaml:"banned_phrases"`
	OncePhrases      []string `yaml:"once_phrases"` // allowed at most once per conversation
	NoFixOn          []int    `yaml:"no_fix_on"`    // 0-based turn indexes
	NoFeedbackOn     []int    `yaml:"no_feedback_on"`
	ExpectFix        []Expect `yaml:"expect_fix"`
}

// Expect matches a fix on Turn whose original/suggested text contain the given substrings.
type Expect struct {
	Turn      int    `yaml:"turn"`
	Original  string `yaml:"original"`
	Suggested string `yaml:"suggested"`
}

type TurnResult struct {
	Input    string
	Reply    string
	Feedback []coach.Suggestion
}

type CheckResult struct {
	Name   string
	Pass   bool
	Detail string
}

type CaseResult struct {
	Name       string
	Transcript []TurnResult
	Checks     []CheckResult
}

func Load(path string) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Suite
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return Suite{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s Suite) validate() error {
	var errs []error
	if s.Module == "" {
		errs = append(errs, errors.New("module is required"))
	}
	for _, c := range s.Cases {
		if c.Name == "" || len(c.Turns) == 0 {
			errs = append(errs, fmt.Errorf("case %q: name and turns are required", c.Name))
		}
		for _, e := range c.Checks.ExpectFix {
			if e.Turn < 0 || e.Turn >= len(c.Turns) {
				errs = append(errs, fmt.Errorf("case %q: expect_fix turn %d out of range", c.Name, e.Turn))
			}
		}
	}
	return errors.Join(errs...)
}

// Run plays each case in a fresh session and scores it.
func Run(ctx context.Context, eng *engine.Engine, s Suite) ([]CaseResult, error) {
	var out []CaseResult
	for _, c := range s.Cases {
		sess, err := eng.StartSession(ctx, s.Module)
		if err != nil {
			return nil, err
		}
		var transcript []TurnResult
		for _, input := range c.Turns {
			tr := TurnResult{Input: input}
			var reply strings.Builder
			for ev, err := range eng.Turn(ctx, sess.ID, engine.Input{Text: input}) {
				if err != nil {
					return nil, fmt.Errorf("case %q: %w", c.Name, err)
				}
				switch ev.Kind {
				case engine.EventText:
					reply.WriteString(ev.Text)
				case engine.EventFeedback:
					tr.Feedback = ev.Feedback
				}
			}
			tr.Reply = reply.String()
			transcript = append(transcript, tr)
		}
		out = append(out, CaseResult{Name: c.Name, Transcript: transcript, Checks: Score(c.Checks, transcript)})
	}
	return out, nil
}

// Score applies the checks to a transcript. Leak and non-empty checks always run.
func Score(c Checks, tr []TurnResult) []CheckResult {
	var res []CheckResult
	add := func(name string, bad []string) {
		res = append(res, CheckResult{Name: name, Pass: len(bad) == 0, Detail: strings.Join(bad, "; ")})
	}

	var bad []string
	for i, t := range tr {
		low := strings.ToLower(t.Reply)
		if strings.TrimSpace(t.Reply) == "" {
			bad = append(bad, fmt.Sprintf("turn %d: empty reply", i))
		}
		for _, leak := range []string{"<feedback", "</feedback", "corrections:", "upgrade:", "fix:"} {
			if strings.Contains(low, leak) {
				bad = append(bad, fmt.Sprintf("turn %d: reply contains %q", i, leak))
			}
		}
	}
	add("clean_reply", bad)

	if c.MaxQuestionRatio != nil && len(tr) > 0 {
		n := 0
		for _, t := range tr {
			if strings.HasSuffix(strings.TrimSpace(t.Reply), "?") {
				n++
			}
		}
		ratio := float64(n) / float64(len(tr))
		var bad []string
		if ratio > *c.MaxQuestionRatio {
			bad = []string{fmt.Sprintf("%d/%d replies end with a question (%.0f%% > %.0f%%)", n, len(tr), ratio*100, *c.MaxQuestionRatio*100)}
		}
		add("question_ratio", bad)
	}

	if c.MinUpgradeRatio != nil && len(tr) > 0 {
		var missing []string
		for i, t := range tr {
			if !slices.ContainsFunc(t.Feedback, func(s coach.Suggestion) bool { return s.Kind == coach.KindUpgrade }) {
				missing = append(missing, fmt.Sprint(i))
			}
		}
		ratio := float64(len(tr)-len(missing)) / float64(len(tr))
		var bad []string
		if ratio < *c.MinUpgradeRatio {
			bad = []string{fmt.Sprintf("upgrades on %.0f%% of turns (< %.0f%%); missing on turns %s",
				ratio*100, *c.MinUpgradeRatio*100, strings.Join(missing, ","))}
		}
		add("upgrade_ratio", bad)
	}

	if c.MaxReplyWords > 0 {
		var bad []string
		for i, t := range tr {
			if n := len(strings.Fields(t.Reply)); n > c.MaxReplyWords {
				bad = append(bad, fmt.Sprintf("turn %d: %d words", i, n))
			}
		}
		add("reply_length", bad)
	}

	if len(c.BannedPhrases) > 0 {
		var bad []string
		for i, t := range tr {
			low := normalize(t.Reply)
			for _, p := range c.BannedPhrases {
				if strings.Contains(low, normalize(p)) {
					bad = append(bad, fmt.Sprintf("turn %d: %q", i, p))
				}
			}
		}
		add("banned_phrases", bad)
	}

	if len(c.OncePhrases) > 0 {
		var bad []string
		for _, p := range c.OncePhrases {
			n := 0
			for _, t := range tr {
				n += strings.Count(normalize(t.Reply), normalize(p))
			}
			if n > 1 {
				bad = append(bad, fmt.Sprintf("%q used %d times", p, n))
			}
		}
		add("no_repeated_phrases", bad)
	}

	var quoted []string
	for i, t := range tr {
		low := normalize(t.Reply)
		for _, s := range t.Feedback {
			if s.Kind == coach.KindFix && quotesMistake(low, normalize(s.Original)) {
				quoted = append(quoted, fmt.Sprintf("turn %d: reply discusses the mistake %q", i, s.Original))
			}
		}
	}
	add("reply_ignores_mistakes", quoted)

	if len(c.NoFixOn) > 0 {
		var bad []string
		for _, i := range c.NoFixOn {
			if i < 0 || i >= len(tr) {
				continue
			}
			for _, s := range tr[i].Feedback {
				if s.Kind == coach.KindFix {
					bad = append(bad, fmt.Sprintf("turn %d: unexpected fix %q => %q", i, s.Original, s.Suggested))
				}
			}
		}
		add("no_false_fixes", bad)
	}

	if len(c.NoFeedbackOn) > 0 {
		var bad []string
		for _, i := range c.NoFeedbackOn {
			if i >= 0 && i < len(tr) && len(tr[i].Feedback) > 0 {
				bad = append(bad, fmt.Sprintf("turn %d: unexpected feedback %q => %q", i, tr[i].Feedback[0].Original, tr[i].Feedback[0].Suggested))
			}
		}
		add("no_feedback", bad)
	}

	for i, e := range c.ExpectFix {
		var bad []string
		if e.Turn >= len(tr) || !slices.ContainsFunc(tr[e.Turn].Feedback, e.matches) {
			bad = []string{fmt.Sprintf("turn %d: no fix matching original~%q suggested~%q", e.Turn, e.Original, e.Suggested)}
		}
		add(fmt.Sprintf("expect_fix#%d", i), bad)
	}
	return res
}

// quotesMistake reports whether reply contains the whole mistake, or any two-word
// fragment of it in quotes (e.g. `the "have went" combo`).
func quotesMistake(reply, original string) bool {
	if len(original) > 3 && strings.Contains(reply, original) {
		return true
	}
	w := strings.Fields(original)
	for i := 0; i+1 < len(w); i++ {
		if strings.Contains(reply, `"`+w[i]+" "+w[i+1]+`"`) {
			return true
		}
	}
	return false
}

func (e Expect) matches(s coach.Suggestion) bool {
	return s.Kind == coach.KindFix &&
		strings.Contains(normalize(s.Original), normalize(e.Original)) &&
		strings.Contains(normalize(s.Suggested), normalize(e.Suggested))
}

func normalize(s string) string {
	return strings.ToLower(strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`).Replace(s))
}
