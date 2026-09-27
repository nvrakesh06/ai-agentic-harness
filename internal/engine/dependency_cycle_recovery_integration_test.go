package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// This real-Git publication and replacement-machine attach fixture belongs to
// release execution. It is intentionally not run as worker feedback while a
// shared heavy gate owns the machine.
func TestDependencyCycleRecoveryPublishesAndSurvivesAttach(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stateRef, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []*model.Task{
		cycleRecoveryIntegrationTask("208", model.Ready, []string{"8"}, base),
		cycleRecoveryIntegrationTask("8", model.Ready, []string{"169"}, base),
		cycleRecoveryIntegrationTask("169", model.Superseded, nil, base),
	} {
		snapshot.Tasks[task.ID] = task
	}
	snapshot.Tasks["169"].SupersededBy = "208"
	next, err := f.P.Git.StateCommit(ctx, stateRef, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	updates := []gitx.Update{{Branch: "aih-state", Old: stateRef, New: next}}
	for _, id := range []string{"208", "8", "169"} {
		updates = append(updates, gitx.Update{Branch: snapshot.Tasks[id].Branch, New: base})
	}
	if err = f.P.Git.Publish(ctx, updates); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, snapshot); err != nil {
		t.Fatal(err)
	}
	request := engine.DependencyCycleRecoveryRequest{Schema: 1, CommandID: "cycle-recovery-real-git", ExpectedStateRef: next, ExpectedMainSHA: base, PolicyHash: f.P.Config.Hash, RulesHash: roles.Hash(), OwnerID: "208", DependencyID: "8", OwnerState: model.Ready, OwnerBaseSHA: base, OwnerHeadSHA: base, DependenciesHash: cycleRecoveryDependenciesHash([]string{"8"}), Reason: "Operator removes the one audited edge that closes the successor-aware dependency cycle."}
	if err = engine.RecoverDependencyCycle(ctx, f.P, request); err != nil {
		t.Fatal(err)
	}
	// The typed snapshot receipt, rather than owner decision text, makes the
	// exact command retry idempotent after the edge has gone.
	if err = engine.RecoverDependencyCycle(ctx, f.P, request); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.Open(ctx, filepath.Join(f.Root, "machine-b"))
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.DB.Close()
	if err = replacement.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := replacement.Git.Load(ctx)
	if err != nil || len(recovered.Tasks["208"].Dependencies) != 0 || !recovered.Applied[request.CommandID] {
		t.Fatalf("recovery publication was not portable: %#v (%v)", recovered.Tasks["208"], err)
	}
	if receipt, ok := recovered.DependencyCycleRecoveryReceipts[request.CommandID]; !ok || receipt.OwnerID != "208" || receipt.DependencyID != "8" {
		t.Fatal("supervisor recovery receipt was not durable")
	}
}

func cycleRecoveryIntegrationTask(id string, state model.State, dependencies []string, base string) *model.Task {
	return &model.Task{ID: id, ObjectiveID: "objective", Title: id, Objective: "cycle recovery fixture", Acceptance: []string{"preserve the dependency-cycle recovery contract"}, Dependencies: dependencies, Areas: []string{"README.md"}, AssignedAreas: []string{"README.md"}, AssignedAreaKinds: map[string]string{"README.md": model.AreaFile}, Domains: []string{"fixture"}, Risk: "low", State: state, Branch: "aih/" + id, BaseSHA: base, HeadSHA: base, FixCycles: map[string]int{}}
}

func cycleRecoveryDependenciesHash(dependencies []string) string {
	// Keep this external-package fixture independent from the engine's private
	// digest helper while preserving the exact JSON-array contract.
	encoded, _ := json.Marshal(dependencies)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// This is intentionally a real-Git fixture, kept for the coordinated recovery
// gate. It proves a stopped controller cannot publish around a retained merge.
func TestDependencyCycleRecoveryRejectsRetainedWorktreeMerge(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	base, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, stateRef, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []*model.Task{
		cycleRecoveryIntegrationTask("208", model.Ready, []string{"8"}, base),
		cycleRecoveryIntegrationTask("8", model.Ready, []string{"169"}, base),
		cycleRecoveryIntegrationTask("169", model.Superseded, nil, base),
	} {
		snapshot.Tasks[task.ID] = task
	}
	snapshot.Tasks["169"].SupersededBy = "208"
	next, err := f.P.Git.StateCommit(ctx, stateRef, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	updates := []gitx.Update{{Branch: "aih-state", Old: stateRef, New: next}}
	for _, id := range []string{"208", "8", "169"} {
		updates = append(updates, gitx.Update{Branch: snapshot.Tasks[id].Branch, New: base})
	}
	if err = f.P.Git.Publish(ctx, updates); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Save(next, snapshot); err != nil {
		t.Fatal(err)
	}
	path := f.P.TaskPath(snapshot.Tasks["8"])
	if err = f.P.Git.Worktree(ctx, path, snapshot.Tasks["8"].Branch, base); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "README.md"), []byte("task branch conflict\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktree := gitx.Git{Dir: path}
	if _, err = worktree.Run(ctx, "", "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err = worktree.Run(ctx, "", "commit", "-m", "task conflict fixture"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.Source, "README.md"), []byte("main conflict\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	if _, err = source.Run(ctx, "", "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "commit", "-m", "main conflict fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = worktree.Run(ctx, "", "merge", "--no-commit", "origin/main"); err == nil {
		t.Fatal("fixture did not create a retained merge")
	}
	main, err := f.P.Git.SHA(ctx, "refs/remotes/origin/main")
	if err != nil {
		t.Fatal(err)
	}
	request := engine.DependencyCycleRecoveryRequest{Schema: 1, CommandID: "cycle-recovery-pending-merge", ExpectedStateRef: next, ExpectedMainSHA: main, PolicyHash: f.P.Config.Hash, RulesHash: roles.Hash(), OwnerID: "208", DependencyID: "8", OwnerState: model.Ready, OwnerBaseSHA: base, OwnerHeadSHA: base, DependenciesHash: cycleRecoveryDependenciesHash([]string{"8"}), Reason: "Operator removes the one audited edge that closes the proven successor-aware cycle."}
	if err = engine.PreviewDependencyCycleRecovery(ctx, f.P, request); err == nil || !strings.Contains(err.Error(), "unresolved worktree merge") {
		t.Fatalf("retained MERGE_HEAD was accepted: %v", err)
	}
	metadataPath, err := worktree.Run(ctx, "", "rev-parse", "--git-path", "MERGE_HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(metadataPath) {
		metadataPath = filepath.Join(path, metadataPath)
	}
	// Even invalid MERGE_HEAD bytes retain a pending-merge record. The recovery
	// path must use existence rather than parsing the ref to prove this fence.
	if err = os.WriteFile(metadataPath, []byte{0xff, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = engine.PreviewDependencyCycleRecovery(ctx, f.P, request); err == nil || !strings.Contains(err.Error(), "unresolved worktree merge") {
		t.Fatalf("malformed retained MERGE_HEAD was accepted: %v", err)
	}
	// Break the metadata parent after the malformed-record proof. rev-parse or
	// Lstat must fail closed; clean worktree status cannot substitute for it.
	if err = os.RemoveAll(filepath.Dir(metadataPath)); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Dir(metadataPath), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = engine.PreviewDependencyCycleRecovery(ctx, f.P, request); err == nil {
		t.Fatal("MERGE_HEAD metadata path inspection failure was accepted")
	}
}
