package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Store struct{ DB *sql.DB }
type Command struct{ ID, Kind, Target, Payload string }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// SQLite otherwise creates a database using the process umask (often 0644).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) migrate() error {
	var v int
	if err := s.DB.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > 1 {
		return fmt.Errorf("local schema %d is newer than runtime", v)
	}
	_, err := s.DB.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA synchronous=FULL;
	CREATE TABLE IF NOT EXISTS snapshot (id INTEGER PRIMARY KEY CHECK(id=1), head TEXT NOT NULL, body BLOB NOT NULL);
	CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, at TEXT NOT NULL, task TEXT, run TEXT, role TEXT, provider TEXT, kind TEXT NOT NULL, message TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS commands (id TEXT PRIMARY KEY, kind TEXT NOT NULL, target TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'queued', error TEXT NOT NULL DEFAULT '');
	CREATE TABLE IF NOT EXISTS runtime (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	PRAGMA user_version=1;`)
	return err
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Save(head string, snap *model.Snapshot) error {
	b, e := json.Marshal(snap)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT INTO snapshot(id,head,body) VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET head=excluded.head,body=excluded.body", head, b)
	return e
}
func (s *Store) Load() (*model.Snapshot, string, error) {
	var h string
	var b []byte
	e := s.DB.QueryRow("SELECT head,body FROM snapshot WHERE id=1").Scan(&h, &b)
	if e != nil {
		return nil, "", e
	}
	snap, _, e := model.Decode(b)
	return snap, h, e
}
func (s *Store) Event(task, run, role, provider, kind, msg string) error {
	_, e := s.DB.Exec("INSERT INTO events(at,task,run,role,provider,kind,message) VALUES(?,?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339Nano), task, run, role, provider, kind, msg)
	return e
}
func (s *Store) Submit(c Command) error {
	_, e := s.DB.Exec("INSERT INTO commands(id,kind,target,payload) VALUES(?,?,?,?)", c.ID, c.Kind, c.Target, c.Payload)
	return e
}
func (s *Store) Pending() ([]Command, error) {
	rows, e := s.DB.Query("SELECT id,kind,target,payload FROM commands WHERE status='queued' ORDER BY rowid")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		if e = rows.Scan(&c.ID, &c.Kind, &c.Target, &c.Payload); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) Ack(id, errText string) error {
	status := "accepted"
	if errText != "" {
		status = "failed"
	}
	_, e := s.DB.Exec("UPDATE commands SET status=?,error=? WHERE id=?", status, errText, id)
	return e
}
func (s *Store) Set(key, value string) error {
	_, e := s.DB.Exec("INSERT INTO runtime(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return e
}
func (s *Store) Get(key string) string {
	var v string
	_ = s.DB.QueryRow("SELECT value FROM runtime WHERE key=?", key).Scan(&v)
	return v
}
