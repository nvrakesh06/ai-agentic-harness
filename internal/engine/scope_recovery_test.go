package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestScopeRecoveryExplicitReauthorizationAndIdempotence(t *testing.T) {
	ctx := context.Background()
	f, manifest, before := scopeRecoveryFixture(t, ctx, "legacy-task", nil, nil)
	defer f.P.DB.Close()
	s, stateRef, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyScopeDecisions(87)
	s.Tasks["legacy-task"].Decisions = append([]string(nil), legacy...)
	seeded, err := f.P.Git.StateCommit(ctx, stateRef, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateRef, New: seeded}}); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(seeded, s); err != nil {
		t.Fatal(err)
	}
	manifest.ExpectedStateRef, before = seeded, seeded
	if err := engine.RecoverScope(ctx, f.P, manifest); err != nil {
		t.Fatal(err)
	}
	after, afterRef, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := after.Tasks["legacy-task"]
	if task.State != model.Blocked || task.Blocker == nil || task.AssignedAreaKinds["README.md"] != model.AreaFile || !after.Applied[manifest.CommandID] {
		t.Fatalf("explicit recovery did not preserve blocked task and publish assignment: %#v", task)
	}
	if task.Evidence != nil || task.Summary != "preserve source summary" || task.Verification == nil {
		t.Fatalf("recovery did not invalidate only stale evidence: %#v", task)
	}
	if len(task.Decisions) != len(legacy)+1 || !reflect.DeepEqual(task.Decisions[:len(legacy)], legacy) || !strings.HasPrefix(task.Decisions[len(legacy)], "AIH_SCOPE_RECOVERY_V1:") {
		t.Fatalf("missing typed recovery decision: %#v", task.Decisions)
	}
	if afterRef == before {
		t.Fatal("successful recovery did not publish state")
	}
	if err = engine.RecoverScope(ctx, f.P, manifest); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	retried, retryRef, err := f.P.Git.Load(ctx)
	if err != nil || retryRef != afterRef || !reflect.DeepEqual(retried.Tasks["legacy-task"].Decisions, task.Decisions) {
		t.Fatalf("idempotent retry published a new state ref: %s -> %s (%v)", afterRef, retryRef, err)
	}
}

func legacyScopeDecisions(count int) []string {
	decisions := make([]string, count)
	for i := range decisions {
		decisions[i] = fmt.Sprintf("legacy decision %d", i)
	}
	return decisions
}

func TestScopeRecoveryAllowsStoppedRunAndPreflightHistory(t *testing.T) {
	ctx := context.Background()
	f, manifest, _ := scopeRecoveryFixture(t, ctx, "legacy-task", nil, nil)
	defer f.P.DB.Close()
	s, h, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := s.Tasks["legacy-task"]
	task.State, task.Blocker, task.RunID = model.Ready, nil, "interrupted-run"
	task.Preflight = &model.Preflight{Phase: "writing", BaseSHA: task.BaseSHA, HeadSHA: task.HeadSHA, Config: f.P.Config.Hash, Rules: roles.Hash()}
	s.Runs = append(s.Runs, model.Run{ID: "interrupted-run", Task: task.ID, Outcome: "interrupted"})
	next, err := f.P.Git.StateCommit(ctx, h, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); err != nil {
		t.Fatal(err)
	}
	manifest.ExpectedStateRef = next
	if err = engine.RecoverScope(ctx, f.P, manifest); err != nil {
		t.Fatalf("stopped continuation history was treated as active: %v", err)
	}
}

func TestScopeRecoveryReauthorizesExplicitUnknownLegacyAssignment(t *testing.T) {
	ctx := context.Background()
	f, manifest, _ := scopeRecoveryFixture(t, ctx, "legacy-task", map[string]string{"README.md": model.AreaUnknown}, nil)
	defer f.P.DB.Close()
	if err := engine.RecoverScope(ctx, f.P, manifest); err != nil {
		t.Fatal(err)
	}
	s, _, err := f.P.Git.Load(ctx)
	if err != nil || s.Tasks["legacy-task"].AssignedAreaKinds["README.md"] != model.AreaFile {
		t.Fatalf("unknown legacy assignment was not replaced by explicit authorization: %#v (%v)", s.Tasks["legacy-task"], err)
	}
}

func TestScopeRecoveryRejectsInvalidOrUnsafeContractsBeforePublication(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		change func(*demo.Fixture, *engine.ScopeRecoveryManifest)
	}{
		{"wrong state ref", func(_ *demo.Fixture, m *engine.ScopeRecoveryManifest) { m.ExpectedStateRef = strings.Repeat("a", 40) }},
		{"wrong policy hash", func(_ *demo.Fixture, m *engine.ScopeRecoveryManifest) { m.PolicyHash = strings.Repeat("b", 64) }},
		{"scope escape", func(_ *demo.Fixture, m *engine.ScopeRecoveryManifest) { m.Tasks[0].Areas = []string{"outside.txt"} }},
		{"known assignment overwrite", func(f *demo.Fixture, m *engine.ScopeRecoveryManifest) {
			s, h, err := f.P.Git.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.Tasks["legacy-task"].AssignedAreas = []string{"README.md"}
			s.Tasks["legacy-task"].AssignedAreaKinds = map[string]string{"README.md": model.AreaFile}
			next, err := f.P.Git.StateCommit(ctx, h, s)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); err != nil {
				t.Fatal(err)
			}
			m.ExpectedStateRef = next
		}},
		{"live owner", func(f *demo.Fixture, m *engine.ScopeRecoveryManifest) {
			s, h, err := f.P.Git.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s.Controller = model.Lease{Machine: "other", Owner: "live-owner", Epoch: 3, Heartbeat: time.Now().UTC(), Expires: time.Now().UTC().Add(time.Minute)}
			next, err := f.P.Git.StateCommit(ctx, h, s)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: h, New: next}}); err != nil {
				t.Fatal(err)
			}
			m.ExpectedStateRef = next
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, manifest, _ := scopeRecoveryFixture(t, ctx, "legacy-task", nil, nil)
			defer f.P.DB.Close()
			test.change(f, &manifest)
			_, before, err := f.P.Git.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = engine.RecoverScope(ctx, f.P, manifest); err == nil {
				t.Fatal("unsafe recovery was accepted")
			}
			_, after, loadErr := f.P.Git.Load(ctx)
			if loadErr != nil || after != before {
				t.Fatalf("rejected recovery published remote state: %s -> %s (%v)", before, after, loadErr)
			}
		})
	}
}

func TestScopeRecoveryOverlapDependencyAndCycle(t *testing.T) {
	ctx := context.Background()
	t.Run("overlap needs predecessor", func(t *testing.T) {
		f, manifest, _ := scopeRecoveryFixture(t, ctx, "legacy-task", nil, func(s *model.Snapshot, base, head string) {
			s.Tasks["owner-task"] = &model.Task{ID: "owner-task", State: model.Ready, BaseSHA: base, HeadSHA: head, Branch: "aih/owner-task", AssignedAreas: []string{"README.md"}, AssignedAreaKinds: map[string]string{"README.md": model.AreaFile}, FixCycles: map[string]int{}}
		})
		defer f.P.DB.Close()
		_, before, err := f.P.Git.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.RecoverScope(ctx, f.P, manifest); err == nil {
			t.Fatal("overlap without dependency was accepted")
		}
		_, after, _ := f.P.Git.Load(ctx)
		if after != before {
			t.Fatal("overlap rejection published state")
		}
		manifest.CommandID = "scope-overlap-dependency"
		manifest.ExpectedStateRef = before
		manifest.Tasks[0].AdditionalDependencies = []string{"owner-task"}
		if err = engine.RecoverScope(ctx, f.P, manifest); err != nil {
			t.Fatalf("declared predecessor dependency was rejected: %v", err)
		}
	})
	t.Run("cycle rejected", func(t *testing.T) {
		f, manifest, _ := scopeRecoveryFixture(t, ctx, "legacy-task", nil, func(s *model.Snapshot, base, head string) {
			s.Tasks["owner-task"] = &model.Task{ID: "owner-task", State: model.Ready, BaseSHA: base, HeadSHA: head, Branch: "aih/owner-task", Dependencies: []string{"legacy-task"}, AssignedAreas: []string{"README.md"}, AssignedAreaKinds: map[string]string{"README.md": model.AreaFile}, FixCycles: map[string]int{}}
		})
		defer f.P.DB.Close()
		manifest.Tasks[0].AdditionalDependencies = []string{"owner-task"}
		_, before, err := f.P.Git.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = engine.RecoverScope(ctx, f.P, manifest); err == nil {
			t.Fatal("cycle was accepted")
		}
		_, after, _ := f.P.Git.Load(ctx)
		if after != before {
			t.Fatal("cycle rejection published state")
		}
	})
	t.Run("existing reverse predecessor remains serialized", func(t *testing.T) {
		f, manifest, _ := scopeRecoveryFixture(t, ctx, "legacy-task", nil, func(s *model.Snapshot, base, head string) {
			s.Tasks["owner-task"] = &model.Task{ID: "owner-task", State: model.Ready, BaseSHA: base, HeadSHA: head, Branch: "aih/owner-task", Dependencies: []string{"legacy-task"}, AssignedAreas: []string{"README.md"}, AssignedAreaKinds: map[string]string{"README.md": model.AreaFile}, FixCycles: map[string]int{}}
		})
		defer f.P.DB.Close()
		if err := engine.RecoverScope(ctx, f.P, manifest); err != nil {
			t.Fatalf("existing predecessor serialization was rejected: %v", err)
		}
		s, _, err := f.P.Git.Load(ctx)
		if err != nil || len(s.Tasks["owner-task"].Dependencies) != 1 || s.Tasks["owner-task"].Dependencies[0] != "legacy-task" {
			t.Fatalf("reverse predecessor dependency was not preserved: %#v (%v)", s.Tasks["owner-task"], err)
		}
	})
}

func scopeRecoveryFixture(t *testing.T, ctx context.Context, id string, assignment map[string]string, extend func(*model.Snapshot, string, string)) (*demo.Fixture, engine.ScopeRecoveryManifest, string) {
	t.Helper()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	task := &model.Task{ID: id, ObjectiveID: "objective", Title: "legacy", Objective: "explicit legacy reauthorization", Acceptance: []string{"README carries explicit recovered scope test"}, Areas: []string{"mutable-legacy-area"}, State: model.Blocked, Branch: "aih/" + id, BaseSHA: base, FixCycles: map[string]int{}, Summary: "preserve source summary", Verification: &model.Verification{Environment: "native", HeadSHA: base, Fingerprint: strings.Repeat("d", 64), Attempts: 1}, Evidence: &model.Evidence{}}
	model.Block(task, "Stay blocked", "legacy scope needs explicit authorization", model.Ready)
	if assignment != nil {
		task.AssignedAreas = []string{"README.md"}
		task.AssignedAreaKinds = assignment
	}
	worktree := f.P.TaskPath(task)
	if err = f.P.Git.Worktree(ctx, worktree, task.Branch, base); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(worktree, "README.md"), []byte("explicit legacy scope recovery\n"), 0600); err != nil {
		t.Fatal(err)
	}
	head, err := f.P.Git.Checkpoint(ctx, worktree, id)
	if err != nil {
		t.Fatal(err)
	}
	task.HeadSHA = head
	if task.Verification != nil {
		task.Verification.HeadSHA = head
	}
	s, stateRef, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Objectives["objective"] = &model.Objective{ID: "objective", Planned: true}
	s.Tasks[id] = task
	if extend != nil {
		extend(s, base, head)
	}
	next, err := f.P.Git.StateCommit(ctx, stateRef, s)
	if err != nil {
		t.Fatal(err)
	}
	updates := []gitx.Update{{Branch: task.Branch, New: head}, {Branch: "aih-state", Old: stateRef, New: next}}
	if err = f.P.Git.Publish(ctx, updates); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, s); err != nil {
		t.Fatal(err)
	}
	manifest := engine.ScopeRecoveryManifest{Schema: 1, CommandID: "scope-reauth-command", ExpectedStateRef: next, PolicyHash: f.P.Config.Hash, Tasks: []engine.ScopeRecoveryTask{{ID: id, BaseSHA: base, HeadSHA: head, ContractHash: engine.ScopeRecoveryContractHash(task), Areas: []string{"README.md"}, Reason: "Operator explicitly authorizes a new bounded README ownership contract."}}}
	return f, manifest, next
}
