package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func TestThroughputReportsStoppedAndUnavailableHistoryWithoutWrites(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := model.NewSnapshot("project123")
	s.Capacity.ActiveWriters = 2
	s.Capacity.TargetWriters = 2
	s.Capacity.MaxWriters = 2
	s.Capacity.MaxReaders = 2
	s.Capacity.BacklogSource = "queued_objectives"
	s.Tasks["task"] = &model.Task{ID: "task", State: model.Ready}
	ref := strings.Repeat("a", 40)
	if err = db.Save(ref, s); err != nil {
		t.Fatal(err)
	}
	p := &engine.Project{Dir: t.TempDir(), DB: db}
	cmd := New()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err = showThroughput(cmd, p, true, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var report struct {
		LocalActiveWriters    int                    `json:"local_active_writers"`
		LocalSupervisorActive bool                   `json:"local_supervisor_active"`
		Tasks                 []model.TaskThroughput `json:"tasks"`
		UtilizationAvailable  bool                   `json:"historical_writer_utilization_available"`
	}
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.LocalSupervisorActive || report.LocalActiveWriters != 0 || report.UtilizationAvailable || len(report.Tasks) != 1 || report.Tasks[0].TimingAvailable {
		t.Fatalf("unobserved capacity or timing claimed: %s", out.String())
	}
	_, after, err := db.Load()
	if err != nil || after != ref {
		t.Fatalf("report changed cached state: %s %v", after, err)
	}
	out.Reset()
	if err = showThroughput(cmd, p, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state timing unavailable") || !strings.Contains(out.String(), "not wall time") {
		t.Fatalf("human report omits precision: %s", out.String())
	}
}
