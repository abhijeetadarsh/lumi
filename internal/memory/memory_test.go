package memory

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/abhijeetadarsh/lumi/internal/provider/llm"
	"github.com/abhijeetadarsh/lumi/internal/store"
)

type fakeStore struct {
	facts      []store.Fact
	reflected  map[string]bool
	touched    []string
	upserted   []store.Fact
	recentTurn []store.Turn
}

func newFakeStore() *fakeStore {
	return &fakeStore{reflected: map[string]bool{}}
}

func (f *fakeStore) UpsertFact(_ context.Context, fact store.Fact) error {
	f.upserted = append(f.upserted, fact)
	for i, existing := range f.facts {
		if fact.Key != "" && existing.Key == fact.Key {
			f.facts[i] = fact
			return nil
		}
	}
	f.facts = append(f.facts, fact)
	return nil
}

func (f *fakeStore) Facts(_ context.Context, limit int) ([]store.Fact, error) {
	if limit > len(f.facts) {
		limit = len(f.facts)
	}
	return f.facts[:limit], nil
}

func (f *fakeStore) TouchFacts(_ context.Context, ids ...string) error {
	f.touched = append(f.touched, ids...)
	return nil
}

func (f *fakeStore) CountFacts(context.Context) (int, error) { return len(f.facts), nil }

func (f *fakeStore) RecentTurns(context.Context, string, int) ([]store.Turn, error) {
	return f.recentTurn, nil
}

func (f *fakeStore) MarkReflected(_ context.Context, id string, _ int, _ time.Time) error {
	f.reflected[id] = true
	return nil
}

func (f *fakeStore) AlreadyReflected(_ context.Context, id string) (bool, error) {
	return f.reflected[id], nil
}

type fakeLLM struct{ reply string }

func (f fakeLLM) Chat(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Text: f.reply}, nil
}
func (f fakeLLM) Stream(context.Context, llm.Request) (iter.Seq2[llm.Chunk, error], error) {
	return func(func(llm.Chunk, error) bool) {}, nil
}
func (f fakeLLM) Caps() llm.Capabilities { return llm.Capabilities{} }

type fakeResolver struct{ target llm.Target }

func (r fakeResolver) Resolve(llm.Tier) (llm.Target, error) { return r.target, nil }

func newMem(s Store, reply string) *Memory {
	tgt := llm.Target{LLM: fakeLLM{reply: reply}, Model: "cheap-model"}
	return New(s, fakeResolver{target: tgt}, DefaultOptions())
}

func turns(n int) []store.Turn {
	var out []store.Turn
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, store.Turn{Role: role, Content: "I am a backend engineer working in Go and Rust"})
	}
	return out
}

func TestReflectStoresFacts(t *testing.T) {
	s := newFakeStore()
	s.recentTurn = turns(6)
	m := newMem(s, `[{"fact":"Works as a backend engineer in Go","key":"role","confidence":90},
	                 {"fact":"Lives in Toronto","key":"city","confidence":80}]`)

	n, err := m.Reflect(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("kept %d facts, want 2", n)
	}
	if len(s.upserted) != 2 {
		t.Errorf("upserted %d facts", len(s.upserted))
	}
}

func TestReflectIsIdempotent(t *testing.T) {
	s := newFakeStore()
	s.recentTurn = turns(6)
	m := newMem(s, `[{"fact":"Works as a backend engineer","key":"role","confidence":90}]`)

	if _, err := m.Reflect(context.Background(), "sess-1"); err != nil {
		t.Fatal(err)
	}
	before := len(s.upserted)
	// A second reflect of the same session must not learn it again, or quitting
	// and resuming would double-count every fact.
	n, err := m.Reflect(context.Background(), "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second reflect kept %d facts, want 0", n)
	}
	if len(s.upserted) != before {
		t.Errorf("second reflect wrote %d more facts", len(s.upserted)-before)
	}
}

func TestReflectSkipsShortSessions(t *testing.T) {
	s := newFakeStore()
	s.recentTurn = turns(2) // below MinTurns
	m := newMem(s, `[{"fact":"Something","key":"k","confidence":90}]`)
	n, err := m.Reflect(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(s.upserted) != 0 {
		t.Errorf("short session was reflected on: %d facts", n)
	}
}

func TestReflectToleratesProseAroundJSON(t *testing.T) {
	s := newFakeStore()
	s.recentTurn = turns(6)
	// Models often wrap JSON in prose or fences; that must not lose the facts.
	m := newMem(s, "Here are the facts:\n```json\n[{\"fact\":\"Lives in Toronto\",\"key\":\"city\",\"confidence\":80}]\n```\nHope that helps!")

	n, err := m.Reflect(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("kept %d facts, want 1 (JSON was wrapped in prose)", n)
	}
}

func TestReflectRejectsJunkFacts(t *testing.T) {
	s := newFakeStore()
	s.recentTurn = turns(6)
	m := newMem(s, `[
		{"fact":"x","key":"a","confidence":50},
		{"fact":"   ","key":"b","confidence":50},
		{"fact":"This one sentence is deliberately far longer than the one hundred and sixty character limit that the system prompt asks for so it must be rejected outright and thrown away without being stored at all anywhere","key":"c","confidence":50},
		{"fact":"Lives in Toronto","key":"city","confidence":80}
	]`)
	n, err := m.Reflect(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("kept %d facts, want only the 1 valid one", n)
	}
}

// Some models answer the reflection prompt with bare strings instead of the
// requested objects. Losing every fact in that case is what made an early
// version of this store nothing at all, so it is pinned here.
func TestParseFactsAcceptsBareStrings(t *testing.T) {
	got := parseFacts(`["User's name is Rahul", "I live in Bangalore", "Works as a backend engineer"]`, 8)
	if len(got) != 3 {
		t.Fatalf("parsed %d facts from bare strings, want 3: %+v", len(got), got)
	}
	if got[1].Fact != "Lives in Bangalore" {
		t.Errorf("fact = %q, want the first person normalised to third person", got[1].Fact)
	}
}

func TestParseFactsIgnoresNonStringNonObject(t *testing.T) {
	got := parseFacts(`[42, null, {"fact":"Lives in Toronto","key":"city","confidence":80}]`, 8)
	if len(got) != 1 || got[0].Fact != "Lives in Toronto" {
		t.Errorf("got %+v, want only the valid object", got)
	}
}

func TestParseFactsDedupesKeys(t *testing.T) {
	// Within one reflection the first statement for a key wins, so a model that
	// restates itself cannot produce two contradicting facts in one pass.
	got := parseFacts(`[{"fact":"Lives in Toronto","key":"city","confidence":80},
	                     {"fact":"Lives in Berlin","key":"city","confidence":70}]`, 8)
	if len(got) != 1 {
		t.Fatalf("kept %d facts with the same key, want 1", len(got))
	}
	if got[0].Fact != "Lives in Toronto" {
		t.Errorf("kept %q, want the first fact for a key to win", got[0].Fact)
	}
}

func TestParseFactsHonoursMax(t *testing.T) {
	got := parseFacts(`[{"fact":"Fact one here","key":"a","confidence":9},
	                     {"fact":"Fact two here","key":"b","confidence":9},
	                     {"fact":"Fact three here","key":"c","confidence":9}]`, 2)
	if len(got) != 2 {
		t.Errorf("kept %d facts, want the cap of 2", len(got))
	}
}

func TestRenderRespectsCharCap(t *testing.T) {
	facts := make([]store.Fact, 20)
	for i := range facts {
		facts[i] = store.Fact{
			ID:   string(rune('a' + i)),
			Fact: "This is a durable fact about the user number something",
		}
	}
	headerLines := 1
	for _, cap := range []int{50, 120, 300, 600} {
		block, ids := Render(facts, cap)
		if len(block) > cap {
			t.Errorf("cap %d: block is %d chars", cap, len(block))
		}
		// Lines = header + one per fact, so ids must be lines-1.
		if want := strings.Count(block, "\n") + 1 - headerLines; len(ids) != want {
			t.Errorf("cap %d: %d ids for %d fact lines", cap, len(ids), want)
		}
		// A smaller cap must never fit more facts than a larger one.
		if cap >= 600 && len(ids) == 0 {
			t.Errorf("cap %d: no facts fit despite 20 available", cap)
		}
	}
	// Monotonicity: tightening the cap can only reduce the fact count.
	prev := 1 << 30
	for _, cap := range []int{600, 400, 300, 200, 150} {
		_, ids := Render(facts, cap)
		if len(ids) > prev {
			t.Errorf("cap %d fit %d facts, more than the looser cap's %d", cap, len(ids), prev)
		}
		prev = len(ids)
	}
}

func TestRenderEmpty(t *testing.T) {
	if block, ids := Render(nil, 500); block != "" || ids != nil {
		t.Error("expected nothing for no facts")
	}
	if block, _ := Render([]store.Fact{{Fact: "x"}}, 0); block != "" {
		t.Error("expected nothing for a zero cap")
	}
}

// The whole point of the design: per-turn cost must not grow with history.
func TestInjectCostIsBounded(t *testing.T) {
	s := newFakeStore()
	for i := 0; i < 500; i++ {
		s.facts = append(s.facts, store.Fact{
			ID:   string(rune(i%26) + 'a'),
			Fact: "Durable fact number here about something",
		})
	}
	m := New(s, fakeResolver{}, DefaultOptions())
	block, err := m.Inject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(block) > DefaultOptions().MaxChars {
		t.Errorf("injected %d chars, cap is %d", len(block), DefaultOptions().MaxChars)
	}
	if block == "" {
		t.Error("expected a block")
	}
	if len(s.touched) == 0 {
		t.Error("injected facts were not recorded as used")
	}
}

func TestInjectEmptyWhenNoFacts(t *testing.T) {
	m := New(newFakeStore(), fakeResolver{}, DefaultOptions())
	block, err := m.Inject(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if block != "" {
		t.Errorf("expected no block with no facts, got %q", block)
	}
}

func TestRenderMarksUnconfirmed(t *testing.T) {
	block, _ := Render([]store.Fact{{Fact: "Maybe lives in Berlin", Confidence: 40}}, 500)
	if !strings.Contains(block, "unconfirmed") {
		t.Errorf("low-confidence fact not marked: %q", block)
	}
	block, _ = Render([]store.Fact{{Fact: "Lives in Berlin", Confidence: 90}}, 500)
	if strings.Contains(block, "unconfirmed") {
		t.Errorf("confident fact marked unconfirmed: %q", block)
	}
}