// Package store is the local state database: watches, what each watch has
// already seen, and the events it produced. SQLite via modernc (no cgo).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Kind of watch target. Stored as text so the schema reads well in sqlite3.
const (
	KindSearch = "search"
	KindItem   = "item"
	KindSeller = "seller"
)

// Watch is the persisted form. Target is kind-specific JSON: search params,
// an item id, or a seller id.
type Watch struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Profile     string          `json:"profile"`
	Kind        string          `json:"kind"`
	Target      json.RawMessage `json:"target"`
	Sinks       []string        `json:"sinks"`
	Interval    time.Duration   `json:"interval"`
	Pages       int             `json:"pages"`
	CreatedAt   time.Time       `json:"created_at"`
	LastCheckAt time.Time       `json:"last_check_at,omitzero"`
	Baselined   bool            `json:"baselined"`
	// SinceID is the newest item id the first check saw. Vinted item ids grow
	// with listing time, so a search item above it was listed after the watch
	// started; one below it is an old listing the result order surfaced.
	SinceID int64 `json:"since_id,omitempty"`
}

// Seen is the last known state of one item under one watch.
type Seen struct {
	ID        int64
	Price     float64
	Reserved  bool
	Sold      bool
	Title     string
	Snapshot  json.RawMessage
	FirstSeen time.Time
	LastSeen  time.Time
}

type Event struct {
	ID      int64           `json:"-"`
	Type    string          `json:"type"`
	Watch   string          `json:"watch"`
	Profile string          `json:"profile"`
	At      time.Time       `json:"at"`
	Item    json.RawMessage `json:"item,omitempty"`
	Change  json.RawMessage `json:"change,omitempty"`
}

const schema = `
CREATE TABLE IF NOT EXISTS watches (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  profile TEXT NOT NULL,
  kind TEXT NOT NULL,
  target TEXT NOT NULL,
  sinks TEXT NOT NULL DEFAULT '[]',
  interval_sec INTEGER NOT NULL,
  pages INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  last_check_at TEXT,
  baselined INTEGER NOT NULL DEFAULT 0,
  since_id INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS seen (
  watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  item_id INTEGER NOT NULL,
  price REAL NOT NULL,
  reserved INTEGER NOT NULL,
  sold INTEGER NOT NULL,
  title TEXT NOT NULL,
  snapshot TEXT NOT NULL,
  first_seen TEXT NOT NULL,
  last_seen TEXT NOT NULL,
  PRIMARY KEY (watch_id, item_id)
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  watch_id INTEGER NOT NULL REFERENCES watches(id) ON DELETE CASCADE,
  type TEXT NOT NULL,
  at TEXT NOT NULL,
  item TEXT,
  change TEXT
);
CREATE INDEX IF NOT EXISTS events_watch_at ON events(watch_id, at);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One writer at a time keeps the service timer and a manual check from
	// tripping over each other.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init state db: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

var ErrExists = errors.New("a watch with that name already exists")

func (s *Store) AddWatch(ctx context.Context, w Watch) (Watch, error) {
	sinks, _ := json.Marshal(w.Sinks)
	if w.Sinks == nil {
		sinks = []byte("[]")
	}
	w.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `INSERT INTO watches(name,profile,kind,target,sinks,interval_sec,pages,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		w.Name, w.Profile, w.Kind, string(w.Target), string(sinks), int64(w.Interval/time.Second), w.Pages, w.CreatedAt.Format(time.RFC3339))
	if err != nil {
		if isUnique(err) {
			return w, ErrExists
		}
		return w, err
	}
	w.ID, _ = res.LastInsertId()
	return w, nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE")
}

const watchCols = `id,name,profile,kind,target,sinks,interval_sec,pages,created_at,COALESCE(last_check_at,''),baselined,since_id`

func scanWatch(row interface{ Scan(...any) error }) (Watch, error) {
	var w Watch
	var target, sinks, created, last string
	var interval int64
	var baselined int
	if err := row.Scan(&w.ID, &w.Name, &w.Profile, &w.Kind, &target, &sinks, &interval, &w.Pages, &created, &last, &baselined, &w.SinceID); err != nil {
		return w, err
	}
	w.Target = json.RawMessage(target)
	_ = json.Unmarshal([]byte(sinks), &w.Sinks)
	w.Interval = time.Duration(interval) * time.Second
	w.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if last != "" {
		w.LastCheckAt, _ = time.Parse(time.RFC3339, last)
	}
	w.Baselined = baselined == 1
	return w, nil
}

func (s *Store) Watches(ctx context.Context, profile string) ([]Watch, error) {
	q := `SELECT ` + watchCols + ` FROM watches`
	var args []any
	if profile != "" {
		q += ` WHERE profile = ?`
		args = append(args, profile)
	}
	q += ` ORDER BY name`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

var ErrNotFound = errors.New("no watch with that name")

func (s *Store) Watch(ctx context.Context, name string) (Watch, error) {
	w, err := scanWatch(s.db.QueryRowContext(ctx, `SELECT `+watchCols+` FROM watches WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

func (s *Store) RemoveWatch(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM watches WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RemoveProfileWatches drops every watch that ran under a profile.
func (s *Store) RemoveProfileWatches(ctx context.Context, profile string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM watches WHERE profile = ?`, profile)
	return err
}

func (s *Store) SeenFor(ctx context.Context, watchID int64) (map[int64]Seen, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,price,reserved,sold,title,snapshot,first_seen,last_seen FROM seen WHERE watch_id = ?`, watchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Seen{}
	for rows.Next() {
		var sn Seen
		var reserved, sold int
		var snapshot, first, last string
		if err := rows.Scan(&sn.ID, &sn.Price, &reserved, &sold, &sn.Title, &snapshot, &first, &last); err != nil {
			return nil, err
		}
		sn.Reserved, sn.Sold = reserved == 1, sold == 1
		sn.Snapshot = json.RawMessage(snapshot)
		sn.FirstSeen, _ = time.Parse(time.RFC3339, first)
		sn.LastSeen, _ = time.Parse(time.RFC3339, last)
		out[sn.ID] = sn
	}
	return out, rows.Err()
}

// CommitCheck stores the outcome of one check atomically: seen rows upserted,
// removed rows deleted, events appended, watch marked checked and baselined.
// sinceID is recorded only by the watch's first commit and kept afterwards.
// Callers deliver events to sinks before calling this, so a crash mid-way
// re-emits rather than loses.
func (s *Store) CommitCheck(ctx context.Context, watchID int64, sinceID int64, upserts []Seen, removed []int64, events []Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, sn := range upserts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO seen(watch_id,item_id,price,reserved,sold,title,snapshot,first_seen,last_seen)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(watch_id,item_id) DO UPDATE SET price=excluded.price, reserved=excluded.reserved, sold=excluded.sold,
			  title=excluded.title, snapshot=excluded.snapshot, last_seen=excluded.last_seen`,
			watchID, sn.ID, sn.Price, b2i(sn.Reserved), b2i(sn.Sold), sn.Title, string(sn.Snapshot), now, now); err != nil {
			return err
		}
	}
	for _, id := range removed {
		if _, err := tx.ExecContext(ctx, `DELETE FROM seen WHERE watch_id = ? AND item_id = ?`, watchID, id); err != nil {
			return err
		}
	}
	for _, ev := range events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(watch_id,type,at,item,change) VALUES(?,?,?,?,?)`,
			watchID, ev.Type, ev.At.UTC().Format(time.RFC3339Nano), nullable(ev.Item), nullable(ev.Change)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watches SET last_check_at = ?, since_id = CASE WHEN baselined = 0 THEN ? ELSE since_id END, baselined = 1 WHERE id = ?`, now, sinceID, watchID); err != nil {
		return err
	}
	return tx.Commit()
}

func nullable(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Events returns stored events, newest first. watchName "" means all watches.
func (s *Store) Events(ctx context.Context, profile, watchName string, since time.Time, limit int) ([]Event, error) {
	q := `SELECT e.id, e.type, w.name, w.profile, e.at, COALESCE(e.item,''), COALESCE(e.change,'') FROM events e JOIN watches w ON w.id = e.watch_id WHERE 1=1`
	var args []any
	if profile != "" {
		q += ` AND w.profile = ?`
		args = append(args, profile)
	}
	if watchName != "" {
		q += ` AND w.name = ?`
		args = append(args, watchName)
	}
	if !since.IsZero() {
		q += ` AND e.at >= ?`
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` ORDER BY e.at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ev Event
		var at, item, change string
		if err := rows.Scan(&ev.ID, &ev.Type, &ev.Watch, &ev.Profile, &at, &item, &change); err != nil {
			return nil, err
		}
		ev.At, _ = time.Parse(time.RFC3339Nano, at)
		if item != "" {
			ev.Item = json.RawMessage(item)
		}
		if change != "" {
			ev.Change = json.RawMessage(change)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
