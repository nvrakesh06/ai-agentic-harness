package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSelectIntegrationBatchStableBoundedOrder(t *testing.T) {
	s := batchSnapshot()
	for _, id := range []string{"charlie", "alpha", "bravo", "delta"} {
		s.Tasks[id] = batchTask(id, "internal/"+id, id)
	}
	batch := SelectIntegrationBatch(s, map[string][]string{
		"delta": {"internal/delta/file.go"}, "bravo": {"internal/bravo/file.go"},
		"alpha": {"internal/alpha/file.go"}, "charlie": {"internal/charlie/file.go"},
	})
	if batch == nil || len(batch.Tasks) != 3 {
		t.Fatalf("batch = %#v", batch)
	}
	got := []string{batch.Tasks[0].ID, batch.Tasks[1].ID, batch.Tasks[2].ID}
	if want := []string{"alpha", "bravo", "charlie"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("batch order = %v, want %v", got, want)
	}
	if batch.ID != integrationBatchID(batch) || ValidateIntegrationBatch(s, batch) != nil {
		t.Fatalf("valid deterministic batch rejected: %#v", batch)
	}
	if err := ReserveIntegrationBatch(s, batch); err != nil || s.IntegrationBatch == batch {
		t.Fatalf("reserve = %v, manifest was not copied", err)
	}
}

func TestSelectIntegrationBatchFailsClosedOnConflictsDependenciesAndRisk(t *testing.T) {
	s := batchSnapshot()
	s.Tasks["alpha"] = batchTask("alpha", "internal/shared", "alpha")
	s.Tasks["area-conflict"] = batchTask("area-conflict", "internal/shared/child", "area")
	s.Tasks["domain-conflict"] = batchTask("domain-conflict", "internal/domain", "alpha")
	s.Tasks["path-conflict"] = batchTask("path-conflict", "internal/path", "path")
	s.Tasks["dependency"] = batchTask("dependency", "internal/dependency", "dependency")
	s.Tasks["dependency"].Dependencies = []string{"unfinished"}
	s.Tasks["unfinished"] = &Task{ID: "unfinished", State: Review}
	s.Tasks["risk"] = batchTask("risk", "internal/risk", "risk")
	s.Tasks["risk"].Risk = "medium"
	s.Tasks["safe"] = batchTask("safe", "internal/safe", "safe")
	paths := map[string][]string{
		"alpha": {"internal/shared/file.go"}, "area-conflict": {"internal/shared/child/file.go"},
		"domain-conflict": {"internal/domain/file.go"}, "path-conflict": {"internal/shared/file.go"},
		"dependency": {"internal/dependency/file.go"}, "risk": {"internal/risk/file.go"}, "safe": {"internal/safe/file.go"},
	}
	batch := SelectIntegrationBatch(s, paths)
	if batch == nil || len(batch.Tasks) != 2 || batch.Tasks[0].ID != "alpha" || batch.Tasks[1].ID != "safe" {
		t.Fatalf("conflicting candidates admitted: %#v", batch)
	}
	delete(s.Tasks, "safe")
	if batch := SelectIntegrationBatch(s, paths); batch != nil {
		t.Fatalf("singleton fallback must not wait or reserve: %#v", batch)
	}
}

func TestIntegrationBatchManifestMigrationAndValidation(t *testing.T) {
	legacy := batchSnapshot()
	legacy.Schema = 8
	legacy.IntegrationBatch = &IntegrationBatch{ID: "untrusted"}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	recovered, changed, err := Decode(b)
	if err != nil || !changed || recovered.Schema != StateSchema || recovered.IntegrationBatch != nil {
		t.Fatalf("schema-8 recovery = %#v changed=%t err=%v", recovered, changed, err)
	}
	current := batchSnapshot()
	current.IntegrationBatch = &IntegrationBatch{ID: "untrusted"}
	b, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = Decode(b); err == nil {
		t.Fatal("current malformed manifest accepted")
	}
	valid := SelectIntegrationBatch(batchSnapshotWith("alpha", "bravo"), map[string][]string{"alpha": {"internal/alpha/file.go"}, "bravo": {"internal/bravo/file.go"}})
	if valid == nil {
		t.Fatal("fixture did not produce valid manifest")
	}
	valid.Tasks[1].Paths = []string{"../escape"}
	valid.ID = integrationBatchID(valid)
	if ValidateIntegrationBatch(batchSnapshotWith("alpha", "bravo"), valid) == nil {
		t.Fatal("non-canonical manifest path accepted")
	}
	valid = SelectIntegrationBatch(batchSnapshotWith("alpha", "bravo"), map[string][]string{"alpha": {"internal/alpha/file.go"}, "bravo": {"internal/bravo/file.go"}})
	valid.Tasks[1].HeadSHA = strings.Repeat("f", 40)
	if ValidateIntegrationBatch(batchSnapshotWith("alpha", "bravo"), valid) == nil {
		t.Fatal("stale member head accepted")
	}
}

func batchSnapshot() *Snapshot { return NewSnapshot("project123") }
func batchSnapshotWith(ids ...string) *Snapshot {
	s := batchSnapshot()
	for _, id := range ids {
		s.Tasks[id] = batchTask(id, "internal/"+id, id)
	}
	return s
}

func batchTask(id, area, domain string) *Task {
	base, head := strings.Repeat("a", 40), strings.Repeat("e", 40)
	config, rules, scope := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	return &Task{ID: id, State: MergeReady, Risk: "low", BaseSHA: base, HeadSHA: head, Domains: []string{domain}, AssignedAreas: []string{area}, AssignedAreaKinds: map[string]string{area: AreaDirectory}, Evidence: &Evidence{Base: base, Head: head, Config: config, Rules: rules, ReviewRoster: []string{"qa", "reviewer"}, ReviewScope: scope, ReviewDispositions: map[string]ReviewDisposition{"qa": {Disposition: "completed", SourceHead: head, Runtime: "codex/test"}, "reviewer": {Disposition: "completed", SourceHead: head, Runtime: "codex/test"}}}}
}
