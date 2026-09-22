package store

import (
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDurabilityAndCommands(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	snap := model.NewSnapshot("project123")
	if e = s.Save("abc", snap); e != nil {
		t.Fatal(e)
	}
	if e = s.Event("t", "r", "reviewer", "codex", "transition", "REVIEW"); e != nil {
		t.Fatal(e)
	}
	if e = s.Submit(Command{"id", "run", "", "requirement"}); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if runtime.GOOS != "windows" {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("database permissions", info, err)
		}
	}
	got, h, e := s.Load()
	if e != nil || got.Project != snap.Project || h != "abc" {
		t.Fatal(got, h, e)
	}
	q, e := s.Pending()
	if e != nil || len(q) != 1 {
		t.Fatal(q, e)
	}
	if e = s.Ack("id", ""); e != nil {
		t.Fatal(e)
	}
	q, _ = s.Pending()
	if len(q) != 0 {
		t.Fatal("command not acked")
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&count)
	if count != 1 {
		t.Fatal("event lost")
	}
}
func TestRejectsFutureLocalSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.db")
	s, e := Open(p)
	if e != nil {
		t.Fatal(e)
	}
	s.DB.Exec("PRAGMA user_version=99")
	s.Close()
	if _, e = Open(p); e == nil {
		t.Fatal("future local schema opened")
	}
}
