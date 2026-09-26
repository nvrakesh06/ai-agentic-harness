package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// These tests deliberately call the public one-shot Replan entry point. The
// fixture uses a local bare remote, so branch construction, ancestry checks,
// scope validation, and atomic ref publication are all real Git operations.

func TestReplanPublishesMergedSuccessorAndExactRetryDoesNotReplayBusinessState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, request := replanFixture(t, ctx)
	defer f.P.DB.Close()

	if err := engine.Replan(ctx, f.P, request); err != nil {
		t.Fatal(err)
	}
	after, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertPublishedReplan(t, ctx, f, after, request)

	// Replan acquires and releases a short-lived lease on every invocation. Those
	// state commits are required fencing, but an accepted exact retry must not
	// create another successor, alter the receipt, or republish its source ref.
	prior := model.Clone(after)
	priorSuccessor, err := f.P.Git.RemoteHead(ctx, "aih/repair")
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Replan(ctx, f.P, request); err != nil {
		t.Fatal(err)
	}
	retried, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.P.Git.RemoteHead(ctx, "aih/repair"); err != nil || got != priorSuccessor {
		t.Fatalf("exact retry republished successor: got=%s want=%s err=%v", got, priorSuccessor, err)
	}
	assertSameReplanBusinessState(t, prior, retried)

	changed := request
	changed.Reason = "different operator instruction"
	if err = engine.Replan(ctx, f.P, changed); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed manifest reused command ID: %v", err)
	}
	collided, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSameReplanBusinessState(t, retried, collided)
}

func TestReplanRejectsOutOfScopeCandidateBeforeSuccessorPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, request := replanFixture(t, ctx)
	defer f.P.DB.Close()

	// Construct a candidate which has every source checkpoint but adds a file
	// outside the replacement's immutable scope. This exercises the candidate
	// ancestry and full-scope gate with a real merge commit and remote ref.
	sources := make([]gitx.ReplanCheckpoint, 0, len(request.Sources))
	for _, source := range request.Sources {
		sources = append(sources, gitx.ReplanCheckpoint{BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA})
	}
	head, err := f.P.Git.ReplanBranch(ctx, request.Expected.BaseSHA, sources, "fixture candidate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.P.Git.Run(ctx, "", "branch", "aih/candidate", head); err != nil {
		t.Fatal(err)
	}
	candidateDir := filepath.Join(t.TempDir(), "candidate")
	if err = f.P.Git.Worktree(ctx, candidateDir, "aih/candidate", head); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(candidateDir, "outside.txt"), []byte("outside scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidateHead, err := f.P.Git.Checkpoint(ctx, candidateDir, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih/candidate", New: candidateHead}}); err != nil {
		t.Fatal(err)
	}
	request.CandidateHead = candidateHead

	before, err := f.P.Git.RemoteHead(ctx, "aih-state")
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Replan(ctx, f.P, request); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("out-of-scope candidate accepted: %v", err)
	}
	after, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tasks[request.Replacement.ID] != nil || after.Replans[request.CommandID].ReplacementID != "" {
		t.Fatalf("rejected candidate published a successor: %#v", after)
	}
	if successor, err := f.P.Git.RemoteHead(ctx, "aih/repair"); err != nil || successor != "" {
		t.Fatalf("rejected candidate published successor ref: %q %v", successor, err)
	}
	// The only state commits are acquire/release fencing. In particular, this is
	// not a source-and-state transaction for the rejected successor.
	afterHead, err := f.P.Git.RemoteHead(ctx, "aih-state")
	if err != nil || afterHead == before {
		t.Fatalf("expected only lease lifecycle state around rejection: before=%s after=%s err=%v", before, afterHead, err)
	}
}

func TestReplanRejectsProspectiveSuccessorCycleBeforePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, request := replanFixture(t, ctx)
	defer f.P.DB.Close()

	// alpha will point to repair after supersession; repair would depend on
	// downstream; and downstream already depends on alpha. The cycle exists only
	// in the prospective replacement graph, so it must fail before Git constructs
	// or publishes the successor ref.
	s, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Tasks["downstream"] = &model.Task{ID: "downstream", ObjectiveID: "batch", Title: "downstream", Objective: "fixture", Acceptance: []string{"done"}, Dependencies: []string{"alpha"}, Areas: []string{"feature-downstream.txt"}, AssignedAreas: []string{"feature-downstream.txt"}, AssignedAreaKinds: map[string]string{"feature-downstream.txt": model.AreaFile}, Domains: []string{"downstream"}, Risk: "low", State: model.Ready, Branch: "aih/downstream", BaseSHA: request.Expected.BaseSHA, HeadSHA: request.Expected.BaseSHA, FixCycles: map[string]int{}}
	next, err := f.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}
	request.Expected.StateRef = next
	request.Replacement.Dependencies = []string{"downstream"}

	if err = engine.Replan(ctx, f.P, request); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("prospective successor cycle accepted: %v", err)
	}
	after, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tasks[request.Replacement.ID] != nil || after.Applied[request.CommandID] {
		t.Fatalf("cycle rejection published successor state: %#v", after)
	}
	if successor, err := f.P.Git.RemoteHead(ctx, "aih/repair"); err != nil || successor != "" {
		t.Fatalf("cycle rejection published successor ref: %q %v", successor, err)
	}
}

func TestReplanRejectsStaleStateRefBeforeLeasePublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, request := replanFixture(t, ctx)
	defer f.P.DB.Close()

	before, err := f.P.Git.RemoteHead(ctx, "aih-state")
	if err != nil {
		t.Fatal(err)
	}
	request.Expected.StateRef = strings.Repeat("f", 40)
	if err = engine.Replan(ctx, f.P, request); err == nil || !strings.Contains(err.Error(), "state changed") {
		t.Fatalf("stale state ref was accepted: %v", err)
	}
	after, err := f.P.Git.RemoteHead(ctx, "aih-state")
	if err != nil || after != before {
		t.Fatalf("stale state ref acquired a lease: before=%s after=%s err=%v", before, after, err)
	}
}

func TestAcceptedReplanRetryReconcilesIssueAfterCanonicalConfigAdvances(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f, request := replanFixture(t, ctx)
	defer f.P.DB.Close()

	failing := &replanIssueFailureHub{delegate: f.Hub, failures: 1}
	f.P.Hub = failing
	if err := engine.Replan(ctx, f.P, request); err == nil || !strings.Contains(err.Error(), "injected issue reconciliation") {
		t.Fatalf("expected post-publication issue reconciliation failure, got %v", err)
	}
	published, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if published.Tasks[request.Replacement.ID] == nil || published.Tasks[request.Replacement.ID].Issue != 0 {
		t.Fatalf("replan was not durably published before issue failure: %#v", published.Tasks[request.Replacement.ID])
	}

	advanceCanonicalConfig(t, ctx, f)
	if err = engine.Replan(ctx, f.P, request); err != nil {
		t.Fatalf("accepted retry incorrectly required stale policy: %v", err)
	}
	reconciled, _, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Tasks[request.Replacement.ID].Issue == 0 || failing.calls < 2 {
		t.Fatalf("accepted retry did not reconcile successor issue: task=%#v calls=%d", reconciled.Tasks[request.Replacement.ID], failing.calls)
	}
}

func replanFixture(t *testing.T, ctx context.Context) (*demo.Fixture, engine.ReplanRequest) {
	t.Helper()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	base, tasks := seedBatchMembers(t, ctx, f, []string{"alpha", "bravo"})
	s, stateHead, err := f.P.Git.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		task := s.Tasks[task.ID]
		task.State = model.Blocked
		task.Blocker = &model.Blocker{Question: "Preserve this operator checkpoint", Reason: "fixture replan", Resume: model.SyncRequired}
		// Completed guidance and review evidence are evidence for the historical
		// source checkpoint. The successor must start without either approval.
		task.Preflight = &model.Preflight{Phase: "ready", BaseSHA: base, HeadSHA: task.HeadSHA, Config: f.P.Config.Hash, Rules: roles.Hash(), Completed: []string{"designer"}}
	}
	// This accepted plan has never received a worktree, source ref, worker
	// attempt, or approval. It is superseded with its started siblings but does
	// not contribute a source checkpoint to replay.
	s.Tasks["queued"] = &model.Task{ID: "queued", ObjectiveID: "batch", Title: "queued", Objective: "fixture", Acceptance: []string{"done"}, Areas: []string{"feature-queued.txt"}, AssignedAreas: []string{"feature-queued.txt"}, AssignedAreaKinds: map[string]string{"feature-queued.txt": model.AreaFile}, Domains: []string{"queued"}, Risk: "low", State: model.Ready, Branch: "aih/queued", FixCycles: map[string]int{}}
	next, err := f.P.Git.StateCommit(ctx, stateHead, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Publish(ctx, []gitx.Update{{Branch: "aih-state", Old: stateHead, New: next}}); err != nil {
		t.Fatal(err)
	}

	request := engine.ReplanRequest{
		Schema:      1,
		CommandID:   "repair-command",
		Expected:    engine.ReplanExpected{BaseSHA: base, Config: f.P.Config.Hash, Rules: roles.Hash(), StateRef: next},
		Originals:   []engine.ReplanOriginal{{TaskID: "alpha", State: model.Blocked, HeadSHA: s.Tasks["alpha"].HeadSHA}, {TaskID: "bravo", State: model.Blocked, HeadSHA: s.Tasks["bravo"].HeadSHA}, {TaskID: "queued", State: model.Ready}},
		Sources:     []engine.ReplanSource{{TaskID: "alpha", BaseSHA: base, HeadSHA: s.Tasks["alpha"].HeadSHA, Order: 1}, {TaskID: "bravo", BaseSHA: base, HeadSHA: s.Tasks["bravo"].HeadSHA, Order: 2}},
		Replacement: engine.ReplanReplacement{ID: "repair", Title: "Repair fixture", Objective: "fixture", Acceptance: []string{"done"}, Areas: []string{"feature-alpha.txt", "feature-bravo.txt", "feature-queued.txt"}, Domains: []string{"repair"}, Risk: "low"},
		Reason:      "combine the two blocked fixture checkpoints",
	}
	return f, request
}

func assertPublishedReplan(t *testing.T, ctx context.Context, f *demo.Fixture, snapshot *model.Snapshot, request engine.ReplanRequest) {
	t.Helper()
	next := snapshot.Tasks[request.Replacement.ID]
	if next == nil || next.State != model.Ready || next.Branch != "aih/repair" || next.Replan == nil || next.Preflight != nil || next.Evidence != nil || next.Blocker != nil {
		t.Fatalf("successor did not start as a fresh typed task: %#v", next)
	}
	if receipt := snapshot.Replans[request.CommandID]; !snapshot.Applied[request.CommandID] || receipt.ReplacementID != next.ID || receipt.Digest == "" {
		t.Fatalf("typed replan receipt was not recorded: %#v", receipt)
	}
	if len(next.Replan.Sources) != len(request.Sources) {
		t.Fatalf("successor did not retain every source checkpoint: %#v", next.Replan)
	}
	for index, source := range request.Sources {
		got := next.Replan.Sources[index]
		if got.TaskID != source.TaskID || got.BaseSHA != source.BaseSHA || got.HeadSHA != source.HeadSHA || got.Order != source.Order {
			t.Fatalf("successor source %d = %#v, want %#v", index, got, source)
		}
		remote, err := f.P.Git.RemoteHead(ctx, "aih/"+source.TaskID)
		if err != nil || remote != source.HeadSHA {
			t.Fatalf("original source ref %s changed during replan: got=%s want=%s err=%v", source.TaskID, remote, source.HeadSHA, err)
		}
	}
	for _, id := range []string{"alpha", "bravo", "queued"} {
		old := snapshot.Tasks[id]
		if old.State != model.Superseded || old.SupersededBy != next.ID {
			t.Fatalf("original checkpoint was not preserved as superseded: %#v", old)
		}
	}
	if old := snapshot.Tasks["alpha"]; old.Blocker == nil || old.Preflight == nil {
		t.Fatalf("started source checkpoint lost its historical approval: %#v", old)
	}
	remote, err := f.P.Git.RemoteHead(ctx, next.Branch)
	if err != nil || remote != next.HeadSHA {
		t.Fatalf("successor source ref was not atomically published: remote=%s task=%s err=%v", remote, next.HeadSHA, err)
	}
	for _, path := range []string{"feature-alpha.txt", "feature-bravo.txt"} {
		contents, err := f.P.Git.Show(ctx, next.HeadSHA, path)
		if err != nil || strings.TrimSpace(contents) == "" {
			t.Fatalf("successor lost source patch %s: %q %v", path, contents, err)
		}
	}
}

func assertSameReplanBusinessState(t *testing.T, before, after *model.Snapshot) {
	t.Helper()
	left, right := model.Clone(before), model.Clone(after)
	left.Controller, right.Controller = model.Lease{}, model.Lease{}
	left.Revision, right.Revision = 0, 0
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("retry changed replan business state:\nbefore=%#v\nafter=%#v", left, right)
	}
}

func advanceCanonicalConfig(t *testing.T, ctx context.Context, f *demo.Fixture) {
	t.Helper()
	policy := filepath.Join(f.Source, ".aih", "policies.yaml")
	b, err := os.ReadFile(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(policy, append(b, []byte("\n# fixture config advance\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	source := gitx.Git{Dir: f.Source}
	if _, err = source.Run(ctx, "", "add", ".aih/policies.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "commit", "-m", "Advance fixture policy"); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Run(ctx, "", "push", "origin", "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if err = f.P.Git.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
}

type replanIssueFailureHub struct {
	delegate github.Service
	failures int
	calls    int
}

func (h *replanIssueFailureHub) EnsureIssue(ctx context.Context, key, title, body string) (int, error) {
	h.calls++
	if h.failures > 0 {
		h.failures--
		return 0, errors.New("injected issue reconciliation failure")
	}
	return h.delegate.EnsureIssue(ctx, key, title, body)
}
func (h *replanIssueFailureHub) UpdateIssue(ctx context.Context, n int, body string, closed bool) error {
	return h.delegate.UpdateIssue(ctx, n, body, closed)
}
func (h *replanIssueFailureHub) EnsurePR(ctx context.Context, branch, base, title, body string) (int, error) {
	return h.delegate.EnsurePR(ctx, branch, base, title, body)
}
func (h *replanIssueFailureHub) UpdatePR(ctx context.Context, n int, body string) error {
	return h.delegate.UpdatePR(ctx, n, body)
}
func (h *replanIssueFailureHub) SetPRDraft(ctx context.Context, n int, draft bool) error {
	return h.delegate.SetPRDraft(ctx, n, draft)
}
func (h *replanIssueFailureHub) Pull(ctx context.Context, n int) (github.Pull, error) {
	return h.delegate.Pull(ctx, n)
}
func (h *replanIssueFailureHub) Issues(ctx context.Context) ([]github.Issue, error) {
	return h.delegate.Issues(ctx)
}
