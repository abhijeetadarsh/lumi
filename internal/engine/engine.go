// Package engine runs the turn loop. Every frontend (CLI, REST, voice) consumes
// the same Event stream from Engine.Turn.
package engine

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abhijeetadarsh/lumi/internal/coach"
	"github.com/abhijeetadarsh/lumi/internal/module"
	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
	"github.com/abhijeetadarsh/lumi/internal/store"
)

var ErrEmptyInput = errors.New("engine: empty input")

type EventKind string

const (
	EventText     EventKind = "text"     // speakable reply delta
	EventFeedback EventKind = "feedback" // coaching suggestions; never part of the reply
	EventDone     EventKind = "done"     // turn persisted; Usage set when the provider reports it
)

type Event struct {
	Kind     EventKind
	Text     string
	Feedback []coach.Suggestion
	Usage    *llm.Usage
	Latency  time.Duration
}

type Input struct {
	Text string
	// Transcribed marks Text as speech-to-text output; Spoken means the reply is read aloud.
	// Either selects the module's voice_tier.
	Transcribed bool
	Spoken      bool
}

const (
	transcriptNote = "The user's message is a speech-to-text transcript: ignore spelling, capitalization and punctuation, and allow for misheard words."
	spokenNote     = "Your reply will be read aloud: plain spoken sentences only, no emoji, markdown or lists."
)

type Store interface {
	CreateSession(ctx context.Context, s store.Session) error
	GetSession(ctx context.Context, id string) (store.Session, error)
	EndSession(ctx context.Context, id string, at time.Time) error
	AppendTurns(ctx context.Context, turns ...store.Turn) error
	RecentTurns(ctx context.Context, sessionID string, limit int) ([]store.Turn, error)
}

type Modules interface {
	Get(name string) (module.Spec, error)
}

type Options struct {
	Store        Store
	Modules      Modules
	Resolver     llm.Resolver
	HistoryTurns int
}

type Engine struct {
	store        Store
	modules      Modules
	resolver     llm.Resolver
	historyTurns int
	now          func() time.Time
}

func New(o Options) *Engine {
	return &Engine{
		store:        o.Store,
		modules:      o.Modules,
		resolver:     o.Resolver,
		historyTurns: o.HistoryTurns,
		now:          time.Now,
	}
}

func (e *Engine) StartSession(ctx context.Context, moduleName string) (store.Session, error) {
	spec, err := e.modules.Get(moduleName)
	if err != nil {
		return store.Session{}, err
	}
	sess := store.Session{
		ID:            newID(),
		Module:        spec.Name,
		ModuleVersion: spec.Version,
		Tier:          string(spec.Model.Tier),
		StartedAt:     e.now(),
	}
	if err := e.store.CreateSession(ctx, sess); err != nil {
		return store.Session{}, err
	}
	return sess, nil
}

func (e *Engine) Session(ctx context.Context, id string) (store.Session, error) {
	return e.store.GetSession(ctx, id)
}

func (e *Engine) EndSession(ctx context.Context, id string) error {
	return e.store.EndSession(ctx, id, e.now())
}

// Turn streams the reply to one user input. The exchange is persisted only when the
// stream completes; errors, cancellation, or the consumer stopping early persist nothing.
func (e *Engine) Turn(ctx context.Context, sessionID string, in Input) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		text := strings.TrimSpace(in.Text)
		if text == "" {
			yield(Event{}, ErrEmptyInput)
			return
		}

		sess, err := e.store.GetSession(ctx, sessionID)
		if err != nil {
			yield(Event{}, err)
			return
		}
		spec, err := e.modules.Get(sess.Module)
		if err != nil {
			yield(Event{}, err)
			return
		}
		tier := spec.Model.Tier
		if (in.Transcribed || in.Spoken) && spec.Model.VoiceTier != "" {
			tier = spec.Model.VoiceTier
		}
		target, err := e.resolver.Resolve(tier)
		if err != nil {
			yield(Event{}, fmt.Errorf("resolve tier %q: %w", tier, err))
			return
		}
		history, err := e.store.RecentTurns(ctx, sessionID, e.historyTurns)
		if err != nil {
			yield(Event{}, err)
			return
		}

		msgs := buildMessages(spec.System, history, text)
		if in.Transcribed {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: transcriptNote})
		}
		if in.Spoken {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: spokenNote})
		}
		assess := spec.Feedback && coach.Assessable(text)
		switch {
		case assess:
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: coach.Reminder})
		case spec.Feedback:
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: coach.SkipReminder})
		}
		req := llm.Request{
			Model:     target.Model,
			Messages:  msgs,
			MaxTokens: spec.Model.MaxTokens,
			Params:    target.Params,
		}
		if !target.NoTemperature {
			req.Temperature = spec.Model.Temperature
		}

		userAt := e.now()
		stream, err := target.LLM.Stream(ctx, req)
		if err != nil {
			yield(Event{}, err)
			return
		}

		var (
			raw   strings.Builder
			reply strings.Builder
			usage *llm.Usage
			split *coach.Splitter
		)
		if spec.Feedback {
			split = &coach.Splitter{}
		}
		for chunk, err := range stream {
			if err != nil {
				yield(Event{}, err)
				return
			}
			if chunk.Usage != nil {
				usage = chunk.Usage
			}
			raw.WriteString(chunk.Text)
			text := chunk.Text
			if split != nil {
				text = split.Write(text)
			}
			if text == "" {
				continue
			}
			reply.WriteString(text)
			if !yield(Event{Kind: EventText, Text: text}, nil) {
				return
			}
		}
		var feedback []coach.Suggestion
		if split != nil {
			tail, fb := split.Finish()
			if tail != "" && !yield(Event{Kind: EventText, Text: tail}, nil) {
				return
			}
			reply.WriteString(tail)
			if assess {
				feedback = coach.DropEchoes(fb, reply.String())
			}
		}
		doneAt := e.now()

		// Raw content (feedback block included) is persisted so history keeps showing the model its own format.
		assistant := store.Turn{
			ID:        newID(),
			SessionID: sessionID,
			Role:      string(llm.RoleAssistant),
			Content:   raw.String(),
			Model:     target.Model,
			LatencyMS: doneAt.Sub(userAt).Milliseconds(),
			CreatedAt: doneAt,
		}
		if usage != nil {
			assistant.TokensIn, assistant.TokensOut = usage.InputTokens, usage.OutputTokens
		}
		user := store.Turn{ID: newID(), SessionID: sessionID, Role: string(llm.RoleUser), Content: text, CreatedAt: userAt}
		if err := e.store.AppendTurns(ctx, user, assistant); err != nil {
			yield(Event{}, err)
			return
		}

		slog.Debug("turn complete", "session", sessionID, "model", target.Model, "latency_ms", assistant.LatencyMS,
			"tokens_in", assistant.TokensIn, "tokens_out", assistant.TokensOut)
		if len(feedback) > 0 && !yield(Event{Kind: EventFeedback, Feedback: feedback}, nil) {
			return
		}
		yield(Event{Kind: EventDone, Usage: usage, Latency: doneAt.Sub(userAt)}, nil)
	}
}

func buildMessages(system string, history []store.Turn, input string) []llm.Message {
	msgs := make([]llm.Message, 0, len(history)+2)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: system})
	for _, t := range history {
		msgs = append(msgs, llm.Message{Role: llm.Role(t.Role), Content: t.Content})
	}
	return append(msgs, llm.Message{Role: llm.RoleUser, Content: input})
}

// newID returns a time-ordered UUIDv7.
func newID() string { return uuid.Must(uuid.NewV7()).String() }
