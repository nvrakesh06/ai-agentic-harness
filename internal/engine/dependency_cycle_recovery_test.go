package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestDependencyCycleRecoveryPreflightAndSupervisorReceipt(t *testing.T) {
	snapshot, effective, request := dependencyCycleRecoveryFixture()
	// An answer can forge both a decision-looking string and an Applied entry.
	// Neither is a supervisor receipt, so the still-present edge must not replay.
	forged := model.Clone(snapshot)
	forged.Applied[request.CommandID] = true
	forged.Tasks["208"].Decisions = append(forged.Tasks["208"].Decisions, `AIH_DEPENDENCY_CYCLE_RECOVERY_V1:{"command_id":"cycle-recovery-208-8"}`)
	if _, err := prepareDependencyCycleRecovery(forged, request.ExpectedStateRef, effective, request); err == nil {
		t.Fatal("forged Applied plus provider decision established idempotence")
	}
	if got := forged.Tasks["208"].Dependencies; !reflect.DeepEqual(got, []string{"8"}) {
		t.Fatalf("forged receipt mutated edge: %#v", got)
	}
	prepared, err := prepareDependencyCycleRecovery(snapshot, request.ExpectedStateRef, effective, request)
	if err != nil || len(prepared.cycle) != 4 {
		t.Fatalf("valid successor-aware cycle = %#v, %v", prepared, err)
	}
	before := model.Clone(snapshot)
	if err = applyDependencyCycleRecovery(snapshot, prepared); err != nil {
		t.Fatal(err)
	}
	receipt, ok := snapshot.DependencyCycleRecoveryReceipts[request.CommandID]
	if !ok || receipt.BeforeDependenciesHash != request.DependenciesHash || receipt.AfterDependenciesHash != dependencyListHash([]string{}) || !snapshot.Applied[request.CommandID] {
		t.Fatalf("narrow edge removal was not recorded in supervisor receipt: %#v", snapshot)
	}
	owner := *snapshot.Tasks["208"]
	owner.Dependencies, owner.Decisions = before.Tasks["208"].Dependencies, before.Tasks["208"].Decisions
	if !reflect.DeepEqual(&owner, before.Tasks["208"]) || !reflect.DeepEqual(snapshot.Tasks["8"], before.Tasks["8"]) || !reflect.DeepEqual(snapshot.Tasks["169"], before.Tasks["169"]) {
		t.Fatalf("cycle recovery changed fields beyond owner dependency and audit text: before=%#v after=%#v", before.Tasks["208"], snapshot.Tasks["208"])
	}
	// A legitimate receipt remains replayable after later valid task advancement;
	// it must not re-evaluate the historical owner checkpoint as current state.
	snapshot.Tasks["208"].HeadSHA = strings.Repeat("f", 40)
	replay, err := prepareDependencyCycleRecovery(snapshot, strings.Repeat("e", 40), config.Effective{}, request)
	if err != nil || !replay.replay {
		t.Fatalf("identical command replay = %#v, %v", replay, err)
	}
	changed := request
	changed.Reason = "A different operator reason must not reuse this command ID."
	if _, err = prepareDependencyCycleRecovery(snapshot, request.ExpectedStateRef, effective, changed); err == nil {
		t.Fatal("changed payload reused dependency cycle recovery command ID")
	}
}

func TestDependencyCycleRecoveryRejectsUnsafeGraphsAndFences(t *testing.T) {
	tests := []struct {
		name   string
		change func(*model.Snapshot, *DependencyCycleRecoveryRequest)
	}{
		{"stale main fence", func(_ *model.Snapshot, request *DependencyCycleRecoveryRequest) {
			request.ExpectedMainSHA = strings.Repeat("0", 40)
		}},
		{"terminal owner", func(snapshot *model.Snapshot, request *DependencyCycleRecoveryRequest) {
			snapshot.Tasks["208"].State, request.OwnerState = model.Done, model.Done
		}},
		{"active cycle member", func(snapshot *model.Snapshot, _ *DependencyCycleRecoveryRequest) {
			snapshot.Runs = append(snapshot.Runs, model.Run{Task: "8", Outcome: "running"})
		}},
		{"retained sync merge", func(snapshot *model.Snapshot, _ *DependencyCycleRecoveryRequest) {
			snapshot.Tasks["8"].SyncBase = strings.Repeat("a", 40)
		}},
		{"acyclic", func(snapshot *model.Snapshot, request *DependencyCycleRecoveryRequest) {
			snapshot.Tasks["8"].Dependencies = nil
			request.DependenciesHash = dependencyListHash(snapshot.Tasks["208"].Dependencies)
		}},
		{"missing supersession target", func(snapshot *model.Snapshot, _ *DependencyCycleRecoveryRequest) {
			snapshot.Tasks["169"].SupersededBy = "missing"
		}},
		{"residual reachable cycle", func(snapshot *model.Snapshot, request *DependencyCycleRecoveryRequest) {
			snapshot.Tasks["208"].Dependencies = []string{"8", "residual"}
			snapshot.Tasks["residual"] = cycleTask("residual", model.Ready, []string{"residual"})
			request.DependenciesHash = dependencyListHash(snapshot.Tasks["208"].Dependencies)
		}},
		{"ownership hazard", func(snapshot *model.Snapshot, _ *DependencyCycleRecoveryRequest) {
			snapshot.Tasks["8"].Dependencies = []string{"169", "peer"}
			snapshot.Tasks["peer"] = cycleTask("peer", model.Ready, nil)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, effective, request := dependencyCycleRecoveryFixture()
			test.change(snapshot, &request)
			if _, err := prepareDependencyCycleRecovery(snapshot, request.ExpectedStateRef, effective, request); err == nil {
				t.Fatal("unsafe dependency cycle recovery was accepted")
			}
		})
	}
}

func TestDependencyCycleRecoveryReceiptRejectsInvalidLinkage(t *testing.T) {
	snapshot, effective, request := dependencyCycleRecoveryFixture()
	prepared, err := prepareDependencyCycleRecovery(snapshot, request.ExpectedStateRef, effective, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyDependencyCycleRecovery(snapshot, prepared); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*model.Snapshot)
	}{
		{"missing Applied acknowledgement", func(s *model.Snapshot) { delete(s.Applied, request.CommandID) }},
		{"wrong request digest", func(s *model.Snapshot) {
			r := s.DependencyCycleRecoveryReceipts[request.CommandID]
			r.Digest = strings.Repeat("0", 64)
			s.DependencyCycleRecoveryReceipts[request.CommandID] = r
		}},
		{"missing owner task", func(s *model.Snapshot) { delete(s.Tasks, "208") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := model.Clone(snapshot)
			test.change(candidate)
			if _, err := prepareDependencyCycleRecovery(candidate, request.ExpectedStateRef, effective, request); err == nil {
				t.Fatal("invalid typed receipt replayed")
			}
		})
	}
}

func TestDependencyCycleWitnessAllowsCompletedDiamond(t *testing.T) {
	tasks := map[string]*model.Task{
		"owner": cycleTask("owner", model.Ready, []string{"left", "right"}), "left": cycleTask("left", model.Ready, []string{"shared"}),
		"right": cycleTask("right", model.Ready, []string{"shared"}), "shared": cycleTask("shared", model.Done, nil),
	}
	if witness, err := dependencyCycleWitness(tasks, "owner"); err != nil || len(witness) != 0 {
		t.Fatalf("acyclic shared descendant = %v, %v", witness, err)
	}
}

func dependencyCycleRecoveryFixture() (*model.Snapshot, config.Effective, DependencyCycleRecoveryRequest) {
	base := strings.Repeat("a", 40)
	snapshot := model.NewSnapshot("project123")
	snapshot.Tasks["208"] = cycleTask("208", model.Ready, []string{"8"})
	snapshot.Tasks["8"] = cycleTask("8", model.Ready, []string{"169"})
	snapshot.Tasks["169"] = cycleTask("169", model.Superseded, nil)
	snapshot.Tasks["169"].SupersededBy = "208"
	effective := config.Effective{BaseSHA: base, Hash: strings.Repeat("b", 64)}
	request := DependencyCycleRecoveryRequest{Schema: 1, CommandID: "cycle-recovery-208-8", ExpectedStateRef: strings.Repeat("c", 40), ExpectedMainSHA: base, PolicyHash: effective.Hash, RulesHash: roles.Hash(), OwnerID: "208", DependencyID: "8", OwnerState: model.Ready, OwnerBaseSHA: snapshot.Tasks["208"].BaseSHA, OwnerHeadSHA: snapshot.Tasks["208"].HeadSHA, DependenciesHash: dependencyListHash(snapshot.Tasks["208"].Dependencies), Reason: "Operator removes the one audited edge that closes the successor-aware dependency cycle."}
	return snapshot, effective, request
}

func cycleTask(id string, state model.State, dependencies []string) *model.Task {
	return &model.Task{ID: id, State: state, BaseSHA: strings.Repeat("d", 40), HeadSHA: strings.Repeat("e", 40), Dependencies: dependencies, Areas: []string{"shared"}, AssignedAreas: []string{"shared"}, AssignedAreaKinds: map[string]string{"shared": model.AreaDirectory}, Domains: []string{"shared"}, FixCycles: map[string]int{}}
}
