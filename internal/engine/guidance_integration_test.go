package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
)

func TestTaskGuidanceCommandPersistsAndSurvivesAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.P.DB.Close()
	s, head, err := fixture.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mainSHA, err := fixture.P.Git.Run(ctx, "", "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	source := &model.Task{ID: "api", ObjectiveID: "objective", State: model.Done, HeadSHA: mainSHA}
	gate := &model.Task{ID: "gate", ObjectiveID: "objective", State: model.Blocked, Blocker: &model.Blocker{Question: "Hold target", Resume: model.Ready}}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Ready, Dependencies: []string{"gate"}}
	s.Tasks[source.ID], s.Tasks[target.ID], s.Tasks[gate.ID] = source, target, gate
	next, err := fixture.P.Git.StateCommit(ctx, head, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.DB.Save(next, s); err != nil {
		t.Fatal(err)
	}
	controller := engine.New(fixture.P)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	waitStarted(t, fixture.P)
	payload, _ := json.Marshal(map[string]string{"source_task": "api", "text": "Use src/studio-server/cli.ts with --projects-root and --port."})
	if err = fixture.P.DB.Submit(store.Command{ID: "guidance-command", Kind: "guide", Target: "ui", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		persisted, _, loadErr := fixture.P.Git.Load(ctx)
		if loadErr == nil && len(model.TaskGuidance(persisted.Tasks["ui"])) == 1 {
			if guidance := model.TaskGuidance(persisted.Tasks["ui"])[0]; guidance.CommandID != "guidance-command" || guidance.SourceSHA != source.HeadSHA {
				t.Fatalf("wrong durable guidance: %#v", guidance)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = fixture.P.DB.Submit(store.Command{ID: "stop-command", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := fixture.P.DB.Load()
	if err != nil || len(model.TaskGuidance(recovered.Tasks["ui"])) != 1 {
		t.Fatalf("attach lost guidance: %#v %v", recovered.Tasks["ui"], err)
	}
}

func TestOperatorGuidancePersistsAcrossAttachAtExactScope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.P.DB.Close()
	s, stateHead, err := fixture.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := fixture.P.Git.Run(ctx, "", "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih/ui", New: base}}); err != nil {
		t.Fatal(err)
	}
	gate := &model.Task{ID: "gate", ObjectiveID: "objective", State: model.Blocked, Blocker: &model.Blocker{Question: "hold", Resume: model.Ready}}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Fix, Branch: "aih/ui", BaseSHA: base, HeadSHA: base, Dependencies: []string{"gate"}}
	s.Tasks[gate.ID], s.Tasks[target.ID] = gate, target
	next, err := fixture.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.DB.Save(next, s); err != nil {
		t.Fatal(err)
	}
	c := engine.New(fixture.P)
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx) }()
	waitStarted(t, fixture.P)
	payload, _ := json.Marshal(map[string]any{"operator": true, "head": base, "base": base, "config": fixture.P.Config.Hash, "rules": roles.Hash(), "text": "Use the bounded operator contract."})
	if err = fixture.P.DB.Submit(store.Command{ID: "operator-guidance", Kind: "guide", Target: "ui", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		persisted, _, e := fixture.P.Git.Load(ctx)
		if e == nil && len(model.EligibleGuidance(persisted.Tasks["ui"], base, fixture.P.Config.Hash, roles.Hash())) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = fixture.P.DB.Submit(store.Command{ID: "stop", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := fixture.P.DB.Load()
	if err != nil || len(model.EligibleGuidance(recovered.Tasks["ui"], base, fixture.P.Config.Hash, roles.Hash())) != 1 {
		t.Fatalf("operator guidance lost or stale after attach: %#v %v", recovered.Tasks["ui"], err)
	}
}

func TestOperatorGuidanceAcceptsOnlyLiveCanonicalPolicyWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.P.DB.Close()
	s, stateHead, err := fixture.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, err := fixture.P.Git.Run(ctx, "", "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	gate := &model.Task{ID: "gate", ObjectiveID: "objective", State: model.Blocked, Blocker: &model.Blocker{Question: "hold", Resume: model.Ready}}
	target := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Fix, BaseSHA: head, HeadSHA: head, Dependencies: []string{"gate"}}
	s.Tasks[gate.ID], s.Tasks[target.ID] = gate, target
	next, err := fixture.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.DB.Save(next, s); err != nil {
		t.Fatal(err)
	}
	controller := engine.New(fixture.P)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	waitStarted(t, fixture.P)
	policyPath := filepath.Join(fixture.Source, ".aih", "policies.yaml")
	policy, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(policyPath, append(policy, []byte("\n# live policy revision\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	sourceGit := gitx.Git{Dir: fixture.Source}
	if _, err = sourceGit.Run(ctx, "", "add", ".aih/policies.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceGit.Run(ctx, "", "commit", "-m", "Live policy update"); err != nil {
		t.Fatal(err)
	}
	if _, err = sourceGit.Run(ctx, "", "push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := engine.Canonical(ctx, fixture.P.Git)
	if err != nil || live.Hash == fixture.P.Config.Hash {
		t.Fatalf("live canonical policy was not refreshed: %#v %v", live, err)
	}
	payload, _ := json.Marshal(map[string]any{"operator": true, "head": head, "base": live.BaseSHA, "config": live.Hash, "rules": roles.Hash(), "text": "Use the live canonical policy."})
	if err = fixture.P.DB.Submit(store.Command{ID: "live-policy-guidance", Kind: "guide", Target: "ui", Payload: string(payload)}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		persisted, _, loadErr := fixture.P.Git.Load(ctx)
		if loadErr == nil {
			items := model.TaskGuidance(persisted.Tasks["ui"])
			if len(items) == 1 && items[0].Base == live.BaseSHA && items[0].Config == live.Hash {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = fixture.P.DB.Submit(store.Command{ID: "stop", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	persisted, _, err := fixture.P.Git.Load(ctx)
	if err != nil || len(model.TaskGuidance(persisted.Tasks["ui"])) != 1 {
		t.Fatalf("live policy guidance was not accepted by running supervisor: %#v %v", persisted.Tasks["ui"], err)
	}
}

func TestScratchFailurePreservesOperatorGuidanceAcrossAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.P.DB.Close()
	s, stateHead, err := fixture.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := fixture.P.Git.Run(ctx, "", "rev-parse", "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih/ui", New: base}}); err != nil {
		t.Fatal(err)
	}
	task := &model.Task{ID: "ui", ObjectiveID: "objective", Title: "ui", Objective: "exercise bounded guidance recovery", State: model.Ready, Branch: "aih/ui", BaseSHA: base, HeadSHA: base}
	if err = model.QueueOperatorGuidance(task, "operator", base, fixture.P.Config.BaseSHA, fixture.P.Config.Hash, roles.Hash(), "Keep this constraint until an implementer starts."); err != nil {
		t.Fatal(err)
	}
	s.Tasks[task.ID] = task
	next, err := fixture.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.DB.Save(next, s); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(fixture.P.Dir, "scratch"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	controller := engine.New(fixture.P)
	done := make(chan error, 1)
	go func() { done <- controller.Serve(ctx) }()
	waitStarted(t, fixture.P)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		persisted, _, loadErr := fixture.P.Git.Load(ctx)
		if loadErr == nil && persisted.Tasks[task.ID].Attempts > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if fixture.Provider.ImplementationCount(task.Title) != 0 {
		t.Fatal("provider ran despite scratch setup failure")
	}
	if err = fixture.P.DB.Submit(store.Command{ID: "stop", Kind: "handoff"}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = fixture.P.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := fixture.P.DB.Load()
	if err != nil || len(model.EligibleGuidance(recovered.Tasks[task.ID], fixture.P.Config.BaseSHA, fixture.P.Config.Hash, roles.Hash())) != 1 {
		t.Fatalf("scratch failure consumed guidance before restart: %#v %v", recovered.Tasks[task.ID], err)
	}
}
