// Package coach separates coaching feedback from the conversational reply so the
// reply can be spoken or shown as conversation while feedback renders separately.
package coach

import (
	"strings"
	"unicode"
)

const (
	openTag  = "<feedback>"
	closeTag = "</feedback>"
)

// Reminder follows the latest user message each turn; without it, turns with no errors
// teach the model (via history) to stop checking.
const Reminder = "After your reply, write the <feedback> block for the user's latest message: " +
	"a fix line for every real error (if any), then exactly one upgrade line."

// SkipReminder replaces Reminder when the message has nothing to assess (e.g. "x").
const SkipReminder = "The user's latest message is not a real attempt at English " +
	"(a stray letter or keystroke). Reply naturally and briefly; do not write a <feedback> block."

// Assessable reports whether a message contains at least one real word worth coaching.
func Assessable(msg string) bool {
	for _, w := range strings.FieldsFunc(msg, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
		if len([]rune(w)) >= 2 {
			return true
		}
	}
	return false
}

// DropEchoes removes upgrades that merely repeat the assistant's own reply.
func DropEchoes(fb []Suggestion, reply string) []Suggestion {
	r := skeleton(reply)
	out := fb[:0:0]
	for _, s := range fb {
		if s.Kind == KindUpgrade && strings.Contains(r, skeleton(s.Suggested)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

type Kind string

const (
	KindFix     Kind = "fix"
	KindUpgrade Kind = "upgrade"
)

type Suggestion struct {
	Kind      Kind
	Original  string
	Suggested string
	Reason    string
}

// Splitter routes streamed model text: reply text is released as soon as it cannot be
// the start of the feedback tag; everything after the tag is withheld for parsing.
type Splitter struct {
	held     string
	inside   bool
	feedback strings.Builder
}

// Write accepts the next streamed chunk and returns reply text that is safe to emit.
func (s *Splitter) Write(chunk string) string {
	if s.inside {
		s.feedback.WriteString(chunk)
		return ""
	}
	buf := s.held + chunk
	if before, after, ok := strings.Cut(buf, openTag); ok {
		s.inside = true
		s.held = ""
		s.feedback.WriteString(after)
		return strings.TrimRightFunc(before, unicode.IsSpace)
	}
	// Hold back a possible partial tag plus trailing whitespace, which must not be
	// emitted if the feedback block follows.
	rest := buf[:len(buf)-partialSuffix(buf, openTag)]
	out := strings.TrimRightFunc(rest, unicode.IsSpace)
	s.held = buf[len(out):]
	return out
}

// Finish returns reply text still withheld (never the tag) and the parsed feedback.
func (s *Splitter) Finish() (string, []Suggestion) {
	if !s.inside {
		tail := strings.TrimRightFunc(s.held, unicode.IsSpace)
		s.held = ""
		return tail, nil
	}
	body, _, _ := strings.Cut(s.feedback.String(), closeTag)
	return "", ParseFeedback(body)
}

// partialSuffix is the length of the longest proper prefix of tag that buf ends with.
func partialSuffix(buf, tag string) int {
	for n := min(len(tag)-1, len(buf)); n > 0; n-- {
		if strings.HasSuffix(buf, tag[:n]) {
			return n
		}
	}
	return 0
}

// ParseFeedback reads lines of the form
//
//	fix: <original> => <suggested> | <reason>
//	upgrade: <original> => <suggested>
//
// ignoring anything malformed.
func ParseFeedback(body string) []Suggestion {
	var out []Suggestion
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		kind, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k := Kind(strings.ToLower(strings.TrimSpace(kind)))
		if k != KindFix && k != KindUpgrade {
			continue
		}
		orig, sugg, ok := strings.Cut(rest, "=>")
		if !ok {
			continue
		}
		sugg, reason, _ := strings.Cut(sugg, "|")
		s := Suggestion{Kind: k, Original: unquote(orig), Suggested: unquote(sugg), Reason: strings.TrimSpace(reason)}
		if s.Original == "" || s.Suggested == "" || s.Original == s.Suggested {
			continue
		}
		if k == KindFix && cosmetic(s.Original, s.Suggested) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// cosmetic reports whether a fix only changes case, punctuation, spacing or stretched
// letters ("byeee"), which are not worth a learner's attention in casual chat.
func cosmetic(a, b string) bool {
	return skeleton(a) == skeleton(b)
}

func skeleton(s string) string {
	var kept []rune
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			kept = append(kept, r)
		case unicode.IsSpace(r) || r == '-':
			kept = append(kept, ' ')
		}
	}
	var b strings.Builder
	for i := 0; i < len(kept); {
		j := i
		for j < len(kept) && kept[j] == kept[i] {
			j++
		}
		n := j - i
		if n >= 3 {
			n = 1 // stretched letters ("byeee"); real doubles like "ss" in "address" are kept
		}
		for range n {
			b.WriteRune(kept[i])
		}
		i = j
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func unquote(s string) string {
	return strings.Trim(strings.TrimSpace(s), "\"“”`")
}
