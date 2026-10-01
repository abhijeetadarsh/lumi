// Package memory carries durable facts about the user between sessions.
//
// The design goal is a bounded token cost per turn: a session injects a short,
// fixed-size block of facts rather than replaying past transcripts, so prompt
// size does not grow with history. Facts are learned once per session by a
// reflection pass and then capped by count and characters on every read.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
	"github.com/abhijeetadarsh/lumi/internal/store"
)

// Store is the persistence the reflection and injection passes need.
type Store interface {
	UpsertFact(ctx context.Context, f store.Fact) error
	Facts(ctx context.Context, limit int) ([]store.Fact, error)
	TouchFacts(ctx context.Context, ids ...string) error
	CountFacts(ctx context.Context) (int, error)
	RecentTurns(ctx context.Context, sessionID string, limit int) ([]store.Turn, error)
	MarkReflected(ctx context.Context, sessionID string, kept int, at time.Time) error
	AlreadyReflected(ctx context.Context, sessionID string) (bool, error)
}

// Options bounds what reflection may write and what injection may spend.
type Options struct {
	// MaxFacts is how many facts a single reflection may add or update.
	MaxFacts int
	// MaxChars is the hard cap on the injected block. Injection stops early
	// rather than exceeding it, which is what keeps per-turn cost bounded.
	MaxChars int
	// MinTurns is the shortest session worth reflecting on; below this the
	// conversation is too thin to learn anything durable from.
	MinTurns int
	// MaxReflectChars bounds the transcript handed to the reflection model, so
	// a very long session cannot make the reflection call expensive.
	MaxReflectChars int
}

func DefaultOptions() Options {
	return Options{MaxFacts: 8, MaxChars: 600, MinTurns: 4, MaxReflectChars: 6000}
}

const (
	// minFactLen / maxFactLen reject fragments and rambling sentences.
	minFactLen = 8
	maxFactLen = 160
)

// systemPrompt asks for a strict JSON array and nothing else. It is deliberately
// blunt about what not to save: the whole point is to keep the block small.
const systemPrompt = `You extract durable facts about the user from a conversation, so a personal assistant can remember them in future sessions.

Return ONLY a JSON array. Each element: {"fact": string, "key": string, "confidence": int}.

Rules:
- "fact" is one short declarative sentence, at most 160 characters, written in the third person ("Works as a backend engineer in Go").
- "key" is a short stable slug for the topic, lowercase letters/digits/dashes, e.g. "role", "city", "editor". Reuse an existing key when refining a known fact so the new fact replaces the old one instead of contradicting it.
- "confidence" is 0-100: how clearly the user stated it.
- Save only things that will still be true and useful in a later session: identity, role, location, relationships, preferences, goals, ongoing projects, constraints.
- Do NOT save: anything from this conversation only, small talk, the assistant's own suggestions, secrets or credentials, guesses, or anything the user asked to keep transient.
- If nothing durable was learned, return [].
- No prose, no code fences, no explanation.`

type Memory struct {
	store   Store
	opts    Options
	now     func() time.Time
	resolve func(llm.Tier) (llm.Target, error)
}

// Resolver supplies the model used for the reflection pass.
type Resolver interface {
	Resolve(t llm.Tier) (llm.Target, error)
}

func New(s Store, r Resolver, o Options) *Memory {
	return &Memory{store: s, opts: o, resolve: r.Resolve, now: time.Now}
}

type candidate struct {
	Fact       string `json:"fact"`
	Key        string `json:"key"`
	Confidence int    `json:"confidence"`
}

// Reflect reads a finished session and stores the durable facts in it. It is
// called once when a session ends, so the cost is one cheap call per session
// rather than per turn.
//
// The session is only marked reflected once facts have actually been written, so
// a failed or empty reflection is retried on the next run instead of silently
// costing the session its memory forever.
func (m *Memory) Reflect(ctx context.Context, sessionID string) (int, error) {
	if done, err := m.store.AlreadyReflected(ctx, sessionID); err != nil {
		return 0, err
	} else if done {
		return 0, nil
	}

	turns, err := m.store.RecentTurns(ctx, sessionID, 200)
	if err != nil {
		return 0, err
	}
	if len(turns) < m.opts.MinTurns {
		return 0, nil
	}
	transcript := renderTranscript(turns, m.opts.MaxReflectChars)
	if strings.TrimSpace(transcript) == "" {
		return 0, nil
	}

	target, err := m.resolve(llm.TierCheap)
	if err != nil {
		return 0, fmt.Errorf("resolve cheap tier for reflection: %w", err)
	}
	// The tier's params (e.g. reasoning_effort) must be forwarded. Without them
	// this provider can return an empty content field, which would silently
	// teach the assistant nothing while still marking the session reflected.
	resp, err := target.LLM.Chat(ctx, llm.Request{
		Model: target.Model,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: "Conversation:\n\n" + transcript},
		},
		MaxTokens:   600,
		Temperature: ptr(0.0),
		Params:      target.Params,
	})
	if err != nil {
		return 0, fmt.Errorf("reflection: %w", err)
	}

	facts := parseFacts(resp.Text, m.opts.MaxFacts)
	if len(facts) == 0 {
		// Nothing durable was learned (or the model did not follow the format).
		// Still mark it done: an empty result is a legitimate outcome, and
		// retrying forever would cost a call per run for no gain.
		if err := m.store.MarkReflected(ctx, sessionID, 0, m.now()); err != nil {
			return 0, err
		}
		return 0, nil
	}

	now := m.now()
	kept := 0
	for _, c := range facts {
		if err := m.store.UpsertFact(ctx, store.Fact{
			ID:              uuid.Must(uuid.NewV7()).String(),
			Fact:            c.Fact,
			Key:             c.Key,
			Confidence:      c.Confidence,
			SourceSessionID: sessionID,
			CreatedAt:       now,
			UpdatedAt:       now,
		}); err != nil {
			return kept, err
		}
		kept++
	}
	if err := m.store.MarkReflected(ctx, sessionID, kept, m.now()); err != nil {
		return kept, err
	}
	return kept, nil
}

// Inject returns the memory block for a system prompt, already truncated to
// MaxChars, and records that those facts were used.
//
// It returns "" when there is nothing worth injecting, so a first session costs
// no extra tokens at all.
func (m *Memory) Inject(ctx context.Context) (string, error) {
	facts, err := m.store.Facts(ctx, m.opts.MaxFacts*2)
	if err != nil {
		return "", err
	}
	block, used := Render(facts, m.opts.MaxChars)
	if len(used) > 0 {
		if err := m.store.TouchFacts(ctx, used...); err != nil {
			return block, err
		}
	}
	return block, nil
}

// Render formats facts into the injected block, stopping at maxChars. It returns
// the block and the IDs of the facts that fit.
//
// Facts are rendered newest first and the block is hard-capped, so the prompt
// cost of memory is constant no matter how much has been learned.
func Render(facts []store.Fact, maxChars int) (string, []string) {
	if len(facts) == 0 || maxChars <= 0 {
		return "", nil
	}
	header := "What you already know about the user (from earlier sessions; do not repeat it back to them, and do not ask again):"
	if utf8.RuneCountInString(header) >= maxChars {
		return "", nil // no room for the header, so no facts
	}
	var (
		b   strings.Builder
		ids []string
		n   = utf8.RuneCountInString(header)
	)
	b.WriteString(header)
	for _, f := range facts {
		line := "- " + f.Fact
		if f.Confidence > 0 && f.Confidence < 70 {
			line += " (unconfirmed)"
		}
		// The +1 is the newline that follows, and n already counts the header,
		// so the returned block really does respect maxChars.
		if n+utf8.RuneCountInString(line)+1 > maxChars {
			break
		}
		b.WriteString("\n")
		b.WriteString(line)
		n += utf8.RuneCountInString(line) + 1
		ids = append(ids, f.ID)
	}
	if len(ids) == 0 {
		return "", nil
	}
	return b.String(), ids
}

var jsonArray = regexp.MustCompile(`(?s)\[.*?\]`)

// parseFacts extracts the facts from a model reply.
//
// Two shapes are accepted, because models produce both:
//   - objects: {"fact": "...", "key": "...", "confidence": 90}
//   - bare strings: ["User's name is Rahul", ...]
//
// JSON is also routinely wrapped in prose or code fences, so the array is
// located first; a decode failure yields no facts rather than an error.
func parseFacts(text string, max int) []candidate {
	m := jsonArray.FindString(text)
	if m == "" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(m), &items); err != nil {
		return nil
	}
	cands := make([]candidate, 0, len(items))
	for _, item := range items {
		var c candidate
		if err := json.Unmarshal(item, &c); err != nil {
			// Fall back to a bare string, normalised into a third-person fact so
			// stored text reads consistently whichever shape the model chose.
			var s string
			if err := json.Unmarshal(item, &s); err != nil {
				continue
			}
			c = candidate{Fact: normalizeFact(s), Confidence: 100}
		}
		cands = append(cands, c)
	}

	out := make([]candidate, 0, min(len(cands), max))
	seen := map[string]bool{}
	for _, c := range cands {
		f := sanitize(c.Fact)
		if !validFact(f) {
			continue
		}
		key := sanitizeKey(c.Key)
		if key != "" && seen[key] {
			// One fact per key per reflection. The first wins: within a single
			// session the earlier statement is the one the user actually made,
			// and a later duplicate is usually the model restating itself.
			continue
		}
		if key != "" {
			seen[key] = true
		}
		conf := c.Confidence
		if conf <= 0 || conf > 100 {
			conf = 100
		}
		out = append(out, candidate{Fact: f, Key: key, Confidence: conf})
		if len(out) >= max {
			break
		}
	}
	return out
}

// normalizeFact turns a first-person fact into a third-person statement, so
// "I live in Bangalore" is stored as "Lives in Bangalore" and reads correctly
// when injected back into a prompt.
func normalizeFact(s string) string {
	s = sanitize(s)
	lower := strings.ToLower(s)

	// Possessives become plain statements: "My name is Rahul" -> "Name is Rahul".
	for _, p := range []string{"my ", "our "} {
		if strings.HasPrefix(lower, p) {
			return capitalizeFirst(strings.TrimSpace(s[len(p):]))
		}
	}
	if strings.HasPrefix(lower, "we ") {
		return capitalizeFirst(strings.TrimSpace(s[3:]))
	}
	// "I am/is X" already reads fine once "I" is dropped; only bare verbs need
	// the third-person -s, and only when they are not already inflected.
	for _, p := range []string{"i'm ", "im "} {
		if strings.HasPrefix(lower, p) {
			return capitalizeFirst(strings.TrimSpace(s[len(p):]))
		}
	}
	if strings.HasPrefix(lower, "i ") {
		return thirdPerson(strings.TrimSpace(s[2:]))
	}
	return s
}

// thirdPerson inflects a leading bare verb: "live in X" -> "Lives in X".
// It only touches the first word and leaves already-inflected forms alone, so
// "is a backend engineer" is not mangled into "Is a..." plus a stray letter.
func thirdPerson(s string) string {
	if s == "" {
		return s
	}
	word, rest, _ := strings.Cut(s, " ")
	lower := strings.ToLower(word)

	irregular := map[string]string{
		"am": "Is", "is": "Is", "was": "Was", "are": "Are", "were": "Were",
		"have": "Has", "has": "Has", "do": "Does", "does": "Does",
	}
	if v, ok := irregular[lower]; ok {
		return v + " " + rest
	}
	// Already third person, or not a plain verb form.
	if strings.HasSuffix(lower, "s") || !isPlainVerb(lower) {
		return capitalizeFirst(s)
	}
	return capitalizeFirst(word+"s") + " " + rest
}

// isPlainVerb is a cheap heuristic: conjugation only applies to ordinary
// base forms, so words ending in -ing, -ed or -y are left alone.
func isPlainVerb(w string) bool {
	switch {
	case strings.HasSuffix(w, "ing"), strings.HasSuffix(w, "ed"), strings.HasSuffix(w, "ly"):
		return false
	case strings.HasSuffix(w, "y"):
		return false
	}
	return true
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}

var whitespace = regexp.MustCompile(`\s+`)

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	s = whitespace.ReplaceAllString(s, " ")
	return strings.Trim(s, `"'`)
}

func validFact(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < minFactLen || n > maxFactLen {
		return false
	}
	// A fact must contain a couple of words, not a stray token.
	return len(strings.Fields(s)) >= 2
}

var keyClean = regexp.MustCompile(`[^a-z0-9]+`)

func sanitizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = keyClean.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func renderTranscript(turns []store.Turn, maxChars int) string {
	var b strings.Builder
	n := 0
	for _, t := range turns {
		line := t.Role + ": " + collapse(t.Content)
		if n+len(line)+1 > maxChars {
			break
		}
		if n > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line)
		n += len(line) + 1
	}
	return b.String()
}

func collapse(s string) string {
	s = whitespace.ReplaceAllString(strings.TrimSpace(s), " ")
	if len(s) > 600 {
		s = s[:600] + "..."
	}
	return s
}

func ptr[T any](v T) *T { return &v }

// ErrNoFacts is returned by commands that need at least one stored fact.
var ErrNoFacts = errors.New("no facts stored yet")
