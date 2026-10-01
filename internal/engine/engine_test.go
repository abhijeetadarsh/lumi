package engine_test

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/abhijeetadarsh/lumi/internal/coach"
	"github.com/abhijeetadarsh/lumi/internal/engine"
	"github.com/abhijeetadarsh/lumi/internal/module"
	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
	"github.com/abhijeetadarsh/lumi/internal/store"
)

const coachSpec = `
name: coach
version: 2
model: { tier: reasoning, temperature: 0.3 }
system: SYSTEM
`

const feedbackSpec = `
name: tutor
version: 1
model: { tier: reasoning }
feedback: true
system: SYSTEM
`

type fakeLLM struct {
	reqs   []llm.Request
	chunks []llm.Chunk
	err    error
}

func (f *fakeLLM) Stream(_ context.Context, req llm.Request) (iter.Seq2[llm.Chunk, error], error) {
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range f.chunks {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.Request) (llm.Response, error) {
	s, err := f.Stream(ctx, req)
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Collect(s)
}

func (f *fakeLLM) Caps() llm.Capabilities { return llm.Capabilities{} }

type fakeResolver struct{ target llm.Target }

func (r fakeResolver) Resolve(llm.Tier) (llm.Target, error) { return r.target, nil }

func newEngine(t *testing.T, fake *fakeLLM, noTemperature bool) *engine.Engine {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mods, err := module.Load(fstest.MapFS{
		"coach.yaml": {Data: []byte(coachSpec)},
		"tutor.yaml": {Data: []byte(feedbackSpec)},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return engine.New(engine.Options{
		Store:        st,
		Modules:      mods,
		Resolver:     fakeResolver{llm.Target{LLM: fake, Model: "test-model", NoTemperature: noTemperature}},
		HistoryTurns: 10,
	})
}

func runTurn(t *testing.T, eng *engine.Engine, sessionID, text string) ([]engine.Event, error) {
	t.Helper()
	var events []engine.Event
	for ev, err := range eng.Turn(t.Context(), sessionID, engine.Input{Text: text}) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func TestTurnStreamsEventsAndCarriesHistory(t *testing.T) {
	usage := &llm.Usage{InputTokens: 5, OutputTokens: 2}
	fake := &fakeLLM{chunks: []llm.Chunk{{Text: "Hi"}, {Text: " there"}, {Usage: usage}}}
	eng := newEngine(t, fake, false)

	sess, err := eng.StartSession(t.Context(), "coach")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Module != "coach" || sess.ModuleVersion != 2 || sess.Tier != "reasoning" {
		t.Errorf("session = %+v", sess)
	}

	events, err := runTurn(t, eng, sess.ID, "  hello ")
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []engine.Event{
		{Kind: engine.EventText, Text: "Hi"},
		{Kind: engine.EventText, Text: " there"},
		{Kind: engine.EventDone, Usage: usage},
	}
	if diff := cmp.Diff(wantEvents, events, cmpopts.IgnoreFields(engine.Event{}, "Latency")); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}

	if _, err := runTurn(t, eng, sess.ID, "again"); err != nil {
		t.Fatal(err)
	}
	temp := 0.3
	want := llm.Request{
		Model: "test-model",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "SYSTEM"},
			{Role: llm.RoleUser, Content: "hello"},
			{Role: llm.RoleAssistant, Content: "Hi there"},
			{Role: llm.RoleUser, Content: "again"},
		},
		Temperature: &temp,
	}
	if diff := cmp.Diff(want, fake.reqs[1]); diff != "" {
		t.Errorf("second request (-want +got):\n%s", diff)
	}
}

func TestFeedbackIsSeparatedFromReply(t *testing.T) {
	raw := []string{"Lucky you!", " I'd nap.\n\n<feedback>\n", "fix: I has => I have | agreement\n</feedback>"}
	fake := &fakeLLM{}
	for _, r := range raw {
		fake.chunks = append(fake.chunks, llm.Chunk{Text: r})
	}
	eng := newEngine(t, fake, false)
	sess, err := eng.StartSession(t.Context(), "tutor")
	if err != nil {
		t.Fatal(err)
	}

	events, err := runTurn(t, eng, sess.ID, "I has no work")
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Event{
		{Kind: engine.EventText, Text: "Lucky you!"},
		{Kind: engine.EventText, Text: " I'd nap."},
		{Kind: engine.EventFeedback, Feedback: []coach.Suggestion{{Kind: coach.KindFix, Original: "I has", Suggested: "I have", Reason: "agreement"}}},
		{Kind: engine.EventDone},
	}
	if diff := cmp.Diff(want, events, cmpopts.IgnoreFields(engine.Event{}, "Latency")); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}

	if _, err := runTurn(t, eng, sess.ID, "next"); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.reqs[1].Messages[2].Content, strings.Join(raw, ""); got != want {
		t.Errorf("history content = %q, want raw %q", got, want)
	}
}

func TestStrayKeystrokeGetsNoFeedback(t *testing.T) {
	fake := &fakeLLM{chunks: []llm.Chunk{{Text: "Hey, are you there?\n<feedback>\nupgrade: x => Oops, hit send too early!\n</feedback>"}}}
	eng := newEngine(t, fake, false)
	sess, err := eng.StartSession(t.Context(), "tutor")
	if err != nil {
		t.Fatal(err)
	}
	events, err := runTurn(t, eng, sess.ID, "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == engine.EventFeedback || strings.Contains(ev.Text, "feedback") {
			t.Errorf("unexpected event %+v", ev)
		}
	}
	msgs := fake.reqs[0].Messages
	if last := msgs[len(msgs)-1]; last.Role != llm.RoleSystem || !strings.Contains(last.Content, "do not write a <feedback> block") {
		t.Errorf("last message = %+v, want skip reminder", last)
	}
}

func TestTurnOmitsTemperatureWhenUnsupported(t *testing.T) {
	fake := &fakeLLM{chunks: []llm.Chunk{{Text: "ok"}}}
	eng := newEngine(t, fake, true)
	sess, err := eng.StartSession(t.Context(), "coach")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTurn(t, eng, sess.ID, "hi"); err != nil {
		t.Fatal(err)
	}
	if fake.reqs[0].Temperature != nil {
		t.Errorf("temperature = %v, want nil", *fake.reqs[0].Temperature)
	}
}

func TestFailedTurnIsNotPersisted(t *testing.T) {
	fake := &fakeLLM{err: errors.New("boom")}
	eng := newEngine(t, fake, false)
	sess, err := eng.StartSession(t.Context(), "coach")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTurn(t, eng, sess.ID, "lost"); err == nil {
		t.Fatal("want error")
	}

	fake.err, fake.chunks = nil, []llm.Chunk{{Text: "ok"}}
	if _, err := runTurn(t, eng, sess.ID, "kept"); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.reqs[1].Messages); got != 2 {
		t.Errorf("second request has %d messages, want 2 (system + user)", got)
	}
}

func TestTurnRejectsEmptyInput(t *testing.T) {
	eng := newEngine(t, &fakeLLM{}, false)
	sess, err := eng.StartSession(t.Context(), "coach")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTurn(t, eng, sess.ID, "   "); !errors.Is(err, engine.ErrEmptyInput) {
		t.Errorf("err = %v, want ErrEmptyInput", err)
	}
}

func TestStartSessionUnknownModule(t *testing.T) {
	eng := newEngine(t, &fakeLLM{}, false)
	if _, err := eng.StartSession(t.Context(), "nope"); !errors.Is(err, module.ErrNotFound) {
		t.Errorf("err = %v, want module.ErrNotFound", err)
	}
}
