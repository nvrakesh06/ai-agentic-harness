package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

const maxDependencyCycleRecoveryRequestBytes = 32 * 1024

// DependencyCycleRecoveryRequest authorizes exactly one existing OWNER ->
// dependency deletion. It is deliberately not a generic dependency editor.
type DependencyCycleRecoveryRequest struct {
	Schema           int         `json:"schema"`
	CommandID        string      `json:"command_id"`
	ExpectedStateRef string      `json:"expected_state_ref"`
	ExpectedMainSHA  string      `json:"expected_main_sha"`
	PolicyHash       string      `json:"policy_hash"`
	RulesHash        string      `json:"rules_hash"`
	OwnerID          string      `json:"owner_id"`
	DependencyID     string      `json:"dependency_id"`
	OwnerState       model.State `json:"owner_state"`
	OwnerBaseSHA     string      `json:"owner_base_sha"`
	OwnerHeadSHA     string      `json:"owner_head_sha"`
	DependenciesHash string      `json:"dependencies_hash"`
	Reason           string      `json:"reason"`
}

type dependencyCycleRecoveryPrepared struct {
	request DependencyCycleRecoveryRequest
	cycle   []string
	replay  bool
}

func DecodeDependencyCycleRecoveryRequest(r io.Reader) (DependencyCycleRecoveryRequest, error) {
	var request DependencyCycleRecoveryRequest
	data, err := io.ReadAll(io.LimitReader(r, maxDependencyCycleRecoveryRequestBytes+1))
	if err != nil {
		return request, err
	}
	if len(data) == 0 || len(data) > maxDependencyCycleRecoveryRequestBytes {
		return request, errors.New("dependency cycle recovery request must contain 1..32768 bytes")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode dependency cycle recovery request: %w", err)
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return request, errors.New("dependency cycle recovery request must contain one JSON value")
	}
	return request, nil
}

// PreviewDependencyCycleRecovery validates the current remote graph and never
// takes a lease, starts a provider, or publishes a state mutation.
func PreviewDependencyCycleRecovery(ctx context.Context, p *Project, request DependencyCycleRecoveryRequest) error {
	if p == nil {
		return errors.New("dependency cycle recovery requires an open project")
	}
	_, err := previewDependencyCycleRecovery(ctx, p, request)
	return err
}

// RecoverDependencyCycle removes one named edge only after the same exact
// remote proof has been repeated beneath the ordinary supervisor lock.
func RecoverDependencyCycle(ctx context.Context, p *Project, request DependencyCycleRecoveryRequest) error {
	if p == nil {
		return errors.New("dependency cycle recovery requires an open project")
	}
	prepared, err := previewDependencyCycleRecovery(ctx, p, request)
	if err != nil || prepared.replay {
		return err
	}
	lock, err := platform.Acquire(filepath.Join(p.Dir, "supervisor.lock"))
	if err != nil {
		return errors.New("local supervisor is active; dependency cycle recovery requires its ordinary local lock")
	}
	defer lock.Close()

	// Repeat fetch, canonical policy, state, graph, ownership, and idleness
	// proof after the stopped-supervisor lock has excluded a local scheduler.
	prepared, err = previewDependencyCycleRecovery(ctx, p, request)
	if err != nil || prepared.replay {
		return err
	}
	c := New(p)
	if err = c.acquireAndRecoverDependencyCycle(ctx, prepared); err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			_ = c.releaseDependencyCycleRecovery(context.Background())
		}
	}()
	if err = c.releaseDependencyCycleRecovery(ctx); err != nil {
		return err
	}
	released = true
	_ = p.DB.Event("", "", "", "", "dependency_cycle_recovery_applied", "explicit dependency cycle recovery command "+request.CommandID+" published without provider scheduling")
	return nil
}

func previewDependencyCycleRecovery(ctx context.Context, p *Project, request DependencyCycleRecoveryRequest) (dependencyCycleRecoveryPrepared, error) {
	snapshot, stateRef, err := p.Git.Load(ctx)
	if err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	if err = p.Git.Fetch(ctx); err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	effective, err := Canonical(ctx, p.Git)
	if err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	prepared, err := prepareDependencyCycleRecovery(snapshot, stateRef, effective, request)
	if err != nil || prepared.replay {
		return prepared, err
	}
	if err = validateDependencyCycleRecoveryPendingMerges(ctx, p, snapshot, prepared.cycle); err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	return prepared, nil
}

func prepareDependencyCycleRecovery(s *model.Snapshot, stateRef string, effective config.Effective, request DependencyCycleRecoveryRequest) (dependencyCycleRecoveryPrepared, error) {
	if err := validateDependencyCycleRecoveryRequest(request); err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	if replay, err := dependencyCycleRecoveryReceipt(s, request); err != nil || replay {
		return dependencyCycleRecoveryPrepared{request: request, replay: replay}, err
	}
	if request.ExpectedStateRef != stateRef || request.ExpectedMainSHA != effective.BaseSHA || request.PolicyHash != effective.Hash || request.RulesHash != roles.Hash() {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery request does not match the exact remote state, canonical main, policy, or rules")
	}
	if !dependencyCycleRecoveryOwnerState(request.OwnerState) {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery owner must be an explicitly recoverable idle task")
	}
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(time.Now().UTC()) {
		return dependencyCycleRecoveryPrepared{}, fmt.Errorf("%w: held by %s until %s", ErrLease, s.Controller.Machine, s.Controller.Expires)
	}
	owner := s.Tasks[request.OwnerID]
	if owner == nil || owner.State != request.OwnerState || owner.BaseSHA != request.OwnerBaseSHA || owner.HeadSHA != request.OwnerHeadSHA || dependencyListHash(owner.Dependencies) != request.DependenciesHash {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery owner no longer matches its declared state, checkpoint, or dependency digest")
	}
	if !containsSingleDependency(owner.Dependencies, request.DependencyID) {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery names no unique owner dependency")
	}
	if s.Tasks[request.DependencyID] == nil {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery dependency is missing")
	}
	cycle, err := dependencyCycleWitness(s.Tasks, owner.ID)
	if err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	if len(cycle) == 0 || !containsTask(cycle, owner.ID) || !containsTask(cycle, request.DependencyID) {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery edge is not part of an owner-reachable successor-aware cycle")
	}
	if err = dependencyCycleMembersIdle(s, cycle); err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	prospective := model.Clone(s)
	prospective.Tasks[owner.ID].Dependencies = removeDependency(prospective.Tasks[owner.ID].Dependencies, request.DependencyID)
	if residual, residualErr := dependencyCycleWitness(prospective.Tasks, owner.ID); residualErr != nil {
		return dependencyCycleRecoveryPrepared{}, residualErr
	} else if len(residual) != 0 {
		return dependencyCycleRecoveryPrepared{}, errors.New("dependency cycle recovery leaves a reachable successor-aware cycle")
	}
	if err = validateDependencyCycleRecoveryOwnership(prospective, owner.ID); err != nil {
		return dependencyCycleRecoveryPrepared{}, err
	}
	return dependencyCycleRecoveryPrepared{request: request, cycle: cycle}, nil
}

func validateDependencyCycleRecoveryRequest(request DependencyCycleRecoveryRequest) error {
	if request.Schema != 1 || !scopeRecoveryCommandID.MatchString(request.CommandID) || !scopeRecoverySHA.MatchString(request.ExpectedStateRef) || !scopeRecoverySHA.MatchString(request.ExpectedMainSHA) || !scopeRecoveryHash.MatchString(request.PolicyHash) || !scopeRecoveryHash.MatchString(request.RulesHash) || !scopeRecoveryTaskID.MatchString(request.OwnerID) || !scopeRecoveryTaskID.MatchString(request.DependencyID) || request.OwnerID == request.DependencyID || !scopeRecoverySHA.MatchString(request.OwnerBaseSHA) || !scopeRecoverySHA.MatchString(request.OwnerHeadSHA) || !scopeRecoveryHash.MatchString(request.DependenciesHash) || strings.TrimSpace(request.Reason) != request.Reason || request.Reason == "" || len(request.Reason) > model.MaxScopeRecoveryReasonBytes || !utf8.ValidString(request.Reason) || strings.ContainsRune(request.Reason, 0) {
		return errors.New("invalid schema-1 dependency cycle recovery request")
	}
	return safety.Check(request.Reason)
}

func dependencyCycleRecoveryRequestHash(request DependencyCycleRecoveryRequest) string {
	encoded, _ := json.Marshal(request)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func dependencyListHash(dependencies []string) string {
	encoded, _ := json.Marshal(dependencies)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func dependencyCycleRecoveryReceipt(s *model.Snapshot, request DependencyCycleRecoveryRequest) (bool, error) {
	if s == nil {
		return false, nil
	}
	receipt, found := s.DependencyCycleRecoveryReceipts[request.CommandID]
	if !found {
		if s.Applied[request.CommandID] {
			return false, errors.New("dependency cycle recovery Applied command ID lacks a supervisor receipt")
		}
		return false, nil
	}
	if !s.Applied[request.CommandID] {
		return false, errors.New("dependency cycle recovery supervisor receipt lacks Applied acknowledgement")
	}
	if receipt.Operation != "dependency-cycle-recover" || receipt.Digest != dependencyCycleRecoveryRequestHash(request) ||
		receipt.OwnerID != request.OwnerID || receipt.DependencyID != request.DependencyID ||
		receipt.BeforeDependenciesHash != request.DependenciesHash || receipt.StateRef != request.ExpectedStateRef ||
		receipt.MainSHA != request.ExpectedMainSHA || receipt.PolicyHash != request.PolicyHash || receipt.RulesHash != request.RulesHash ||
		receipt.OwnerState != request.OwnerState || receipt.OwnerBaseSHA != request.OwnerBaseSHA || receipt.OwnerHeadSHA != request.OwnerHeadSHA ||
		s.Tasks[receipt.OwnerID] == nil || s.Tasks[receipt.DependencyID] == nil {
		return false, errors.New("dependency cycle recovery command ID was already used by another operation or request")
	}
	return true, nil
}

func containsSingleDependency(dependencies []string, want string) bool {
	count := 0
	for _, dependency := range dependencies {
		if dependency == want {
			count++
		}
	}
	return count == 1
}

func removeDependency(dependencies []string, remove string) []string {
	result := make([]string, 0, len(dependencies)-1)
	for _, dependency := range dependencies {
		if dependency != remove {
			result = append(result, dependency)
		}
	}
	return result
}

func dependencyCycleRecoveryOwnerState(state model.State) bool {
	switch state {
	case model.Ready, model.Fix, model.SyncRequired, model.Blocked:
		return true
	default:
		return false
	}
}

// dependencyCycleWitness follows both ordinary dependency edges and the
// replacement edge of superseded tasks. It validates every reachable target so
// a malformed link cannot be repaired by an unrelated deletion.
func dependencyCycleWitness(tasks map[string]*model.Task, start string) ([]string, error) {
	visiting, done := map[string]bool{}, map[string]bool{}
	stack := []string{}
	var visit func(string) ([]string, error)
	visit = func(id string) ([]string, error) {
		task := tasks[id]
		if task == nil {
			return nil, fmt.Errorf("dependency cycle recovery graph references missing task %s", id)
		}
		if visiting[id] {
			for index, member := range stack {
				if member == id {
					return append(append([]string(nil), stack[index:]...), id), nil
				}
			}
			return nil, errors.New("dependency cycle recovery lost its traversal witness")
		}
		if done[id] {
			return nil, nil
		}
		visiting[id] = true
		stack = append(stack, id)
		edges := append([]string(nil), task.Dependencies...)
		if task.State == model.Superseded {
			if task.SupersededBy == "" {
				return nil, fmt.Errorf("superseded task %s has no replacement", id)
			}
			edges = append(edges, task.SupersededBy)
		}
		seen := map[string]bool{}
		for _, edge := range edges {
			if !scopeRecoveryTaskID.MatchString(edge) || seen[edge] {
				return nil, fmt.Errorf("task %s has malformed or duplicate dependency edge", id)
			}
			seen[edge] = true
			if tasks[edge] == nil {
				return nil, fmt.Errorf("task %s references missing dependency target %s", id, edge)
			}
			if cycle, err := visit(edge); err != nil || len(cycle) != 0 {
				return cycle, err
			}
		}
		delete(visiting, id)
		stack = stack[:len(stack)-1]
		done[id] = true
		return nil, nil
	}
	return visit(start)
}

func dependencyCycleMembersIdle(s *model.Snapshot, cycle []string) error {
	seen := map[string]bool{}
	for _, id := range cycle {
		if seen[id] {
			continue
		}
		seen[id] = true
		task := s.Tasks[id]
		if task == nil || activeScopeRecoveryTask(s, task) {
			return fmt.Errorf("dependency cycle member %s has active work", id)
		}
		if task.SyncBase != "" {
			return fmt.Errorf("dependency cycle member %s retains a pending merge base", id)
		}
		switch task.State {
		case model.Running, model.Implemented, model.Verifying, model.Review, model.MergeReady, model.MergeTrain, model.PostVerify:
			return fmt.Errorf("dependency cycle member %s is not idle", id)
		}
	}
	return nil
}

// validateDependencyCycleRecoveryPendingMerges rejects durable or retained
// merge state for every witness member. A stopped scheduler and expired lease
// do not make an interrupted merge safe to mutate around.
func validateDependencyCycleRecoveryPendingMerges(ctx context.Context, p *Project, s *model.Snapshot, cycle []string) error {
	seen := map[string]bool{}
	for _, id := range cycle {
		if seen[id] {
			continue
		}
		seen[id] = true
		task := s.Tasks[id]
		if task == nil {
			return fmt.Errorf("dependency cycle member %s is missing", id)
		}
		if task.SyncBase != "" {
			return fmt.Errorf("dependency cycle member %s retains a pending merge base", id)
		}
		path := p.TaskPath(task)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect dependency cycle member %s worktree: %w", id, err)
		}
		worktree := gitx.Git{Dir: path}
		if err := validateDependencyCycleRecoveryMergeHead(ctx, worktree, id); err != nil {
			return err
		}
		status, err := worktree.Run(ctx, "", "status", "--porcelain=v1", "--untracked-files=no")
		if err != nil {
			return fmt.Errorf("inspect dependency cycle member %s worktree status: %w", id, err)
		}
		for _, line := range strings.Split(status, "\n") {
			if len(line) < 2 {
				continue
			}
			code := line[:2]
			if code == "DD" || code == "AU" || code == "UD" || code == "UA" || code == "DU" || code == "AA" || code == "UU" {
				return fmt.Errorf("dependency cycle member %s has unresolved worktree merge paths", id)
			}
		}
	}
	return nil
}

// validateDependencyCycleRecoveryMergeHead resolves Git's metadata path from
// the explicit worktree, then uses Lstat so a malformed, unreadable, or
// symlinked MERGE_HEAD is still treated as pending. Only a confirmed absent
// path is safe; rev-parse and filesystem inspection failures are not absence.
func validateDependencyCycleRecoveryMergeHead(ctx context.Context, worktree gitx.Git, taskID string) error {
	metadataPath, err := worktree.Run(ctx, "", "rev-parse", "--git-path", "MERGE_HEAD")
	if err != nil {
		return fmt.Errorf("resolve dependency cycle member %s MERGE_HEAD metadata path: %w", taskID, err)
	}
	if metadataPath == "" || strings.IndexByte(metadataPath, 0) >= 0 || !utf8.ValidString(metadataPath) {
		return fmt.Errorf("resolve dependency cycle member %s MERGE_HEAD metadata path: invalid path", taskID)
	}
	if !filepath.IsAbs(metadataPath) {
		metadataPath = filepath.Join(worktree.Dir, metadataPath)
	}
	metadataPath, err = filepath.Abs(metadataPath)
	if err != nil {
		return fmt.Errorf("resolve dependency cycle member %s MERGE_HEAD metadata path: %w", taskID, err)
	}
	if _, err = os.Lstat(metadataPath); err == nil {
		return fmt.Errorf("dependency cycle member %s has an unresolved worktree merge", taskID)
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return fmt.Errorf("inspect dependency cycle member %s MERGE_HEAD metadata path: %w", taskID, err)
	}
}

// validateDependencyCycleRecoveryOwnership ensures the removed edge does not
// make its owner concurrent with an overlapping unfinished task. Existing
// replacement links remain effective serialization edges.
func validateDependencyCycleRecoveryOwnership(s *model.Snapshot, ownerID string) error {
	owner := s.Tasks[ownerID]
	ownerAreas, known := replanOwnershipAreas(owner)
	if !known {
		return errors.New("dependency cycle recovery cannot prove owner immutable scope")
	}
	for _, task := range s.Tasks {
		if task == nil || task.ID == ownerID || task.State == model.Done || task.State == model.Superseded {
			continue
		}
		areas, known := replanOwnershipAreas(task)
		if !known {
			return fmt.Errorf("dependency cycle recovery cannot prove non-overlap with unfinished legacy task %s", task.ID)
		}
		if (areasOverlap(ownerAreas, areas) || stringsOverlap(owner.Domains, task.Domains)) && !replanSerialized(s.Tasks, ownerID, task.ID) {
			return fmt.Errorf("dependency cycle recovery would unserialize overlapping unfinished task %s", task.ID)
		}
	}
	return nil
}

func applyDependencyCycleRecovery(s *model.Snapshot, prepared dependencyCycleRecoveryPrepared) error {
	if replay, err := dependencyCycleRecoveryReceipt(s, prepared.request); err != nil || replay {
		return err
	}
	owner := s.Tasks[prepared.request.OwnerID]
	if owner == nil || owner.State != prepared.request.OwnerState || owner.BaseSHA != prepared.request.OwnerBaseSHA || owner.HeadSHA != prepared.request.OwnerHeadSHA || dependencyListHash(owner.Dependencies) != prepared.request.DependenciesHash || !containsSingleDependency(owner.Dependencies, prepared.request.DependencyID) {
		return errors.New("dependency cycle recovery owner changed after preflight")
	}
	afterDependencies := removeDependency(owner.Dependencies, prepared.request.DependencyID)
	if s.DependencyCycleRecoveryReceipts == nil {
		s.DependencyCycleRecoveryReceipts = map[string]model.DependencyCycleRecoveryReceipt{}
	}
	if _, exists := s.DependencyCycleRecoveryReceipts[prepared.request.CommandID]; exists || s.Applied[prepared.request.CommandID] {
		return errors.New("dependency cycle recovery command ID is already reserved")
	}
	if len(s.DependencyCycleRecoveryReceipts) >= model.MaxDependencyCycleRecoveryReceipts {
		return errors.New("dependency cycle recovery receipt limit reached")
	}
	owner.Dependencies = afterDependencies
	s.DependencyCycleRecoveryReceipts[prepared.request.CommandID] = model.DependencyCycleRecoveryReceipt{
		Operation: "dependency-cycle-recover", Digest: dependencyCycleRecoveryRequestHash(prepared.request),
		OwnerID: prepared.request.OwnerID, DependencyID: prepared.request.DependencyID,
		BeforeDependenciesHash: prepared.request.DependenciesHash, AfterDependenciesHash: dependencyListHash(afterDependencies),
		StateRef: prepared.request.ExpectedStateRef, MainSHA: prepared.request.ExpectedMainSHA,
		PolicyHash: prepared.request.PolicyHash, RulesHash: prepared.request.RulesHash,
		OwnerState: prepared.request.OwnerState, OwnerBaseSHA: prepared.request.OwnerBaseSHA, OwnerHeadSHA: prepared.request.OwnerHeadSHA,
	}
	// Decisions retain a human audit trail but are never receipt authority.
	owner.Decisions = append(owner.Decisions, "Supervisor dependency-cycle recovery "+prepared.request.CommandID+" removed "+prepared.request.OwnerID+" -> "+prepared.request.DependencyID+".")
	s.Applied[prepared.request.CommandID] = true
	return nil
}

func (c *Controller) acquireAndRecoverDependencyCycle(ctx context.Context, prepared dependencyCycleRecoveryPrepared) error {
	if err := c.P.Git.Fetch(ctx); err != nil {
		return err
	}
	s, head, err := c.P.Git.Load(ctx)
	if err != nil {
		return err
	}
	effective, err := Canonical(ctx, c.P.Git)
	if err != nil {
		return err
	}
	revalidated, err := prepareDependencyCycleRecovery(s, head, effective, prepared.request)
	if err != nil {
		return err
	}
	if revalidated.replay {
		return nil
	}
	if err = validateDependencyCycleRecoveryPendingMerges(ctx, c.P, s, revalidated.cycle); err != nil {
		return err
	}
	now := c.nowUTC()
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(now) {
		return fmt.Errorf("%w: held by %s until %s", ErrLease, s.Controller.Machine, s.Controller.Expires)
	}
	before := model.Clone(s)
	s.Controller = model.Lease{Machine: c.P.Machine.ID, Owner: c.owner, Epoch: s.Controller.Epoch + 1, Heartbeat: now, Expires: now.Add(c.leaseDuration())}
	if err = applyDependencyCycleRecovery(s, revalidated); err != nil {
		return err
	}
	model.AccountTaskTransitions(before, s, now)
	s.Revision++
	next, err := c.P.Git.StateCommit(ctx, head, s)
	if err != nil {
		return err
	}
	publishCtx, cancel, err := c.leasePublicationContext(ctx, s.Controller.Expires)
	if err != nil {
		return err
	}
	defer cancel()
	if err = c.publishUpdates(publishCtx, []gitx.Update{{Branch: "aih-state", Old: head, New: next}}); err != nil {
		return err
	}
	c.s, c.head = s, next
	return c.P.DB.Save(next, s)
}

func (c *Controller) releaseDependencyCycleRecovery(ctx context.Context) error {
	return c.save(ctx, func(s *model.Snapshot) error {
		s.Controller.Owner = ""
		s.Controller.Expires = c.nowUTC()
		return nil
	})
}
