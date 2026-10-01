// Package store persists all personal data in a local SQLite database.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("not found")

// Fixed-width UTC timestamps so text ordering equals time ordering.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

type Session struct {
	ID            string
	Module        string
	ModuleVersion int
	Tier          string
	StartedAt     time.Time
	EndedAt       *time.Time
}

type Turn struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	TokensIn  int
	TokensOut int
	Model     string
	LatencyMS int64
	CreatedAt time.Time
}

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// Single-user, single-process: one connection avoids SQLITE_BUSY between our own writers.
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, module, module_version, tier, started_at) VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.Module, sess.ModuleVersion, sess.Tier, formatTime(sess.StartedAt))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var (
		sess    Session
		started string
		ended   sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, module, module_version, tier, started_at, ended_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Module, &sess.ModuleVersion, &sess.Tier, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("session %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	if sess.StartedAt, err = parseTime(started); err != nil {
		return Session{}, err
	}
	if ended.Valid {
		t, err := parseTime(ended.String)
		if err != nil {
			return Session{}, err
		}
		sess.EndedAt = &t
	}
	return sess, nil
}

func (s *Store) EndSession(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET ended_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("session %q: %w", id, ErrNotFound)
	}
	return nil
}

// AppendTurns writes all turns atomically.
func (s *Store) AppendTurns(ctx context.Context, turns ...Turn) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO turns
		(id, session_id, role, content, tokens_in, tokens_out, model, latency_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, t := range turns {
		if _, err := stmt.ExecContext(ctx, t.ID, t.SessionID, t.Role, t.Content,
			t.TokensIn, t.TokensOut, t.Model, t.LatencyMS, formatTime(t.CreatedAt)); err != nil {
			return fmt.Errorf("append turn: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// RecentTurns returns up to limit most recent turns, oldest first.
func (s *Store) RecentTurns(ctx context.Context, sessionID string, limit int) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, role, content, tokens_in, tokens_out, model, latency_ms, created_at
		FROM turns WHERE session_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query turns: %w", err)
	}
	defer rows.Close()

	var out []Turn
	for rows.Next() {
		var (
			t       Turn
			created string
		)
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Role, &t.Content, &t.TokensIn, &t.TokensOut,
			&t.Model, &t.LatencyMS, &created); err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		if t.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate turns: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}
