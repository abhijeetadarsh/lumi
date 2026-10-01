package store_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/abhijeetadarsh/lumi/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "lumi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var base = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

func TestSessionAndTurnsRoundTrip(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)

	sess := store.Session{ID: "s1", Module: "english-coach", ModuleVersion: 1, Tier: "reasoning", StartedAt: base}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	var turns []store.Turn
	for i := range 5 {
		turns = append(turns, store.Turn{
			ID: fmt.Sprintf("t%d", i), SessionID: "s1", Role: "user", Content: fmt.Sprint(i),
			CreatedAt: base.Add(time.Duration(i) * time.Millisecond),
		})
	}
	if err := s.AppendTurns(ctx, turns...); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentTurns(ctx, "s1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(turns[2:], got); diff != "" {
		t.Errorf("RecentTurns (-want +got):\n%s", diff)
	}

	end := base.Add(time.Hour)
	if err := s.EndSession(ctx, "s1", end); err != nil {
		t.Fatal(err)
	}
	sess.EndedAt = &end
	gotSess, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(sess, gotSess); diff != "" {
		t.Errorf("GetSession (-want +got):\n%s", diff)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	if _, err := openStore(t).GetSession(t.Context(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAppendTurnsIsAtomicAndEnforcesSession(t *testing.T) {
	ctx := t.Context()
	s := openStore(t)
	if err := s.CreateSession(ctx, store.Session{ID: "s1", Module: "m", ModuleVersion: 1, Tier: "fast", StartedAt: base}); err != nil {
		t.Fatal(err)
	}
	err := s.AppendTurns(ctx,
		store.Turn{ID: "ok", SessionID: "s1", Role: "user", Content: "x", CreatedAt: base},
		store.Turn{ID: "orphan", SessionID: "nope", Role: "user", Content: "y", CreatedAt: base},
	)
	if err == nil {
		t.Fatal("AppendTurns succeeded, want foreign key error")
	}
	got, err := s.RecentTurns(ctx, "s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d turns after failed batch, want 0", len(got))
	}
}
