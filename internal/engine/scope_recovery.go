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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

const maxScopeRecoveryManifestBytes = 128 * 1024

var scopeRecoveryCommandID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,120}$`)
var scopeRecoveryTaskID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`)
var scopeRecoverySHA = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var scopeRecoveryHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ScopeRecoveryManifest is intentionally a small, one-shot operator contract.
// It authorizes a new immutable assignment for a legacy task; it never purports
// to infer an old assignment from Areas or a changed-path list.
type ScopeRecoveryManifest struct {
	Schema           int                 `json:"schema"`
	CommandID        string              `json:"command_id"`
	ExpectedStateRef string              `json:"expected_state_ref"`
	PolicyHash       string              `json:"policy_hash"`
	Tasks            []ScopeRecoveryTask `json:"tasks"`
}

type ScopeRecoveryTask struct {
	ID                     string   `json:"id"`
	BaseSHA                string   `json:"base_sha"`
	HeadSHA                string   `json:"head_sha"`
	ContractHash           string   `json:"contract_hash"`
	Areas                  []string `json:"areas"`
	AdditionalDependencies []string `json:"additional_dependencies,omitempty"`
	Reason                 string   `json:"reason"`
}

type scopeRecoveryPrepared struct {
	item         ScopeRecoveryTask
	areas        []gitx.Area
	dependencies []string
}

// DecodeScopeRecoveryManifest accepts one bounded JSON document and rejects
// unknown fields so a future broad repair format cannot silently enter phase 1.
func DecodeScopeRecoveryManifest(r io.Reader) (ScopeRecoveryManifest, error) {
	var manifest ScopeRecoveryManifest
	limited := io.LimitReader(r, maxScopeRecoveryManifestBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return manifest, err
	}
	if len(data) == 0 || len(data) > maxScopeRecoveryManifestBytes {
		return manifest, errors.New("scope recovery manifest must contain 1..131072 bytes")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode scope recovery manifest: %w", err)
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return manifest, errors.New("scope recovery manifest must contain one JSON value")
	}
	return manifest, nil
}

// ScopeRecoveryContractHash binds the existing plan contract before recovery.
// It excludes mutable evidence and Decisions, and includes every field that can
// affect acceptance, ownership, or dependency scheduling.
func ScopeRecoveryContractHash(t *model.Task) string {
	if t == nil {
		return ""
	}
	type contract struct {
		ID           string   `json:"id"`
		ObjectiveID  string   `json:"objective_id"`
		Title        string   `json:"title"`
		Objective    string   `json:"objective"`
		Acceptance   []string `json:"acceptance"`
		Areas        []string `json:"areas"`
		Dependencies []string `json:"dependencies"`
		Domains      []string `json:"domains"`
		Roles        []string `json:"roles"`
		Risk         string   `json:"risk"`
		UI           bool     `json:"ui"`
		Security     bool     `json:"security"`
	}
	payload, _ := json.Marshal(contract{t.ID, t.ObjectiveID, t.Title, t.Objective, t.Acceptance, t.Areas, t.Dependencies, t.Domains, t.Roles, t.Risk, t.UI, t.Security})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// RecoverScope performs no provider work and starts no scheduler. It first
// proves every requested mutation against the exact remote snapshot, canonical
// policy, task checkpoints, and any retained worktree. Only then does it take
// the ordinary local lock and fenced remote lease to persist the new contracts.
func RecoverScope(ctx context.Context, p *Project, manifest ScopeRecoveryManifest) error {
	if p == nil {
		return errors.New("scope recovery requires an open project")
	}
	snapshot, stateRef, _, prepared, err := previewScopeRecovery(ctx, p, manifest)
	if err != nil {
		return err
	}
	if snapshot.Applied[manifest.CommandID] {
		return nil
	}
	lock, err := platform.Acquire(filepath.Join(p.Dir, "supervisor.lock"))
	if err != nil {
		return errors.New("local supervisor is active; scope recovery requires its ordinary local lock")
	}
	defer lock.Close()

	// Re-read after obtaining the local lock. A remote race must fail before the
	// lease publication; the subsequent atomic expected-ref push fences the tiny
	// remaining interval.
	current, currentRef, err := p.Git.Load(ctx)
	if err != nil {
		return err
	}
	if err = p.Git.Fetch(ctx); err != nil {
		return err
	}
	currentEffective, err := Canonical(ctx, p.Git)
	if err != nil {
		return err
	}
	if currentRef != stateRef || currentRef != manifest.ExpectedStateRef || currentEffective.Hash != manifest.PolicyHash {
		return errors.New("scope recovery preflight is stale; regenerate the manifest from the current remote state and policy")
	}
	prepared, err = prepareScopeRecovery(ctx, p, current, currentRef, currentEffective, manifest)
	if err != nil {
		return err
	}

	c := New(p)
	if err = c.acquireAndRecoverScope(ctx, manifest.ExpectedStateRef, manifest, prepared); err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			_ = c.releaseScopeRecovery(context.Background())
		}
	}()
	if err = c.releaseScopeRecovery(ctx); err != nil {
		return err
	}
	released = true
	_ = p.DB.Event("", "", "", "", "scope_recovery_applied", "explicit legacy scope recovery command "+manifest.CommandID+" published without provider scheduling")
	return nil
}

// PreviewScopeRecovery performs the same read-only manifest, checkpoint, and
// worktree proof as recovery. It never takes a lease or publishes remote state.
func PreviewScopeRecovery(ctx context.Context, p *Project, manifest ScopeRecoveryManifest) error {
	if p == nil {
		return errors.New("scope recovery requires an open project")
	}
	_, _, _, _, err := previewScopeRecovery(ctx, p, manifest)
	return err
}

func previewScopeRecovery(ctx context.Context, p *Project, manifest ScopeRecoveryManifest) (*model.Snapshot, string, config.Effective, []scopeRecoveryPrepared, error) {
	snapshot, stateRef, err := p.Git.Load(ctx)
	if err != nil {
		return nil, "", config.Effective{}, nil, err
	}
	if err = p.Git.Fetch(ctx); err != nil {
		return nil, "", config.Effective{}, nil, err
	}
	effective, err := Canonical(ctx, p.Git)
	if err != nil {
		return nil, "", config.Effective{}, nil, err
	}
	prepared, err := prepareScopeRecovery(ctx, p, snapshot, stateRef, effective, manifest)
	if err != nil {
		return nil, "", config.Effective{}, nil, err
	}
	return snapshot, stateRef, effective, prepared, nil
}

func prepareScopeRecovery(ctx context.Context, p *Project, s *model.Snapshot, stateRef string, effective config.Effective, manifest ScopeRecoveryManifest) ([]scopeRecoveryPrepared, error) {
	if manifest.Schema != 1 || !scopeRecoveryCommandID.MatchString(manifest.CommandID) || !scopeRecoverySHA.MatchString(manifest.ExpectedStateRef) || !scopeRecoveryHash.MatchString(manifest.PolicyHash) || len(manifest.Tasks) == 0 || len(manifest.Tasks) > 32 {
		return nil, errors.New("invalid schema-1 scope recovery manifest")
	}
	manifestHash := scopeRecoveryManifestHash(manifest)
	if s.Applied[manifest.CommandID] {
		for _, item := range manifest.Tasks {
			record, ok := model.ScopeRecoveryRecord(s.Tasks[item.ID], manifest.CommandID)
			if !ok || record.ManifestHash != manifestHash {
				return nil, errors.New("scope recovery command ID collides with a different manifest")
			}
		}
		return nil, nil
	}
	if manifest.ExpectedStateRef != stateRef || manifest.PolicyHash != effective.Hash {
		return nil, errors.New("scope recovery manifest does not match the expected remote state ref or canonical policy hash")
	}
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(time.Now().UTC()) {
		return nil, fmt.Errorf("%w: held by %s until %s", ErrLease, s.Controller.Machine, s.Controller.Expires)
	}
	seen := map[string]bool{}
	prepared := make([]scopeRecoveryPrepared, 0, len(manifest.Tasks))
	for _, item := range manifest.Tasks {
		if !scopeRecoveryTaskID.MatchString(item.ID) || seen[item.ID] || !scopeRecoverySHA.MatchString(item.BaseSHA) || !scopeRecoverySHA.MatchString(item.HeadSHA) || !scopeRecoveryHash.MatchString(item.ContractHash) || strings.TrimSpace(item.Reason) != item.Reason || item.Reason == "" || len(item.Reason) > model.MaxScopeRecoveryReasonBytes {
			return nil, errors.New("invalid scope recovery task declaration")
		}
		if err := safety.Check(item.Reason); err != nil {
			return nil, err
		}
		seen[item.ID] = true
		task := s.Tasks[item.ID]
		if task == nil || task.BaseSHA != item.BaseSHA || task.HeadSHA != item.HeadSHA || ScopeRecoveryContractHash(task) != item.ContractHash {
			return nil, fmt.Errorf("scope recovery task %s no longer matches its declared checkpoint or contract", item.ID)
		}
		if task.MergeSHA != "" || task.PostVerifySHA != "" || task.State == model.Done || task.State == model.MergeTrain || task.State == model.PostVerify || string(task.State) == "SUPERSEDED" {
			return nil, fmt.Errorf("scope recovery task %s is merged or in integration", item.ID)
		}
		if activeScopeRecoveryTask(s, task) {
			return nil, fmt.Errorf("scope recovery task %s has active writer, reader, check, review, or integration work", item.ID)
		}
		if !legacyAssignmentRecoverable(task) {
			return nil, fmt.Errorf("scope recovery task %s already has a known immutable assignment", item.ID)
		}
		if len(task.Decisions) >= 64 {
			return nil, fmt.Errorf("scope recovery task %s has no bounded decision capacity", item.ID)
		}
		areas, err := p.Git.ClassifyAreasAtRef(ctx, effective.BaseSHA, item.Areas)
		if err != nil {
			return nil, fmt.Errorf("classify scope recovery task %s areas: %w", item.ID, err)
		}
		if err = p.Git.ValidateCommitScope(ctx, item.BaseSHA, item.HeadSHA, areas); err != nil {
			return nil, fmt.Errorf("validate scope recovery task %s checkpoint: %w", item.ID, err)
		}
		remoteHead, err := p.Git.RemoteHead(ctx, task.Branch)
		if err != nil || remoteHead != item.HeadSHA {
			if err != nil {
				return nil, fmt.Errorf("read scope recovery task %s checkpoint ref: %w", item.ID, err)
			}
			return nil, fmt.Errorf("scope recovery task %s checkpoint ref differs from its declared head", item.ID)
		}
		if err = validateRetainedScopeRecoveryWorktree(ctx, p, task, areas); err != nil {
			return nil, err
		}
		dependencies, err := additionalDependencies(s, task, item.AdditionalDependencies)
		if err != nil {
			return nil, fmt.Errorf("scope recovery task %s dependencies: %w", item.ID, err)
		}
		prepared = append(prepared, scopeRecoveryPrepared{item: item, areas: areas, dependencies: dependencies})
	}
	if err := validateScopeRecoveryOwnership(ctx, p, effective, s, prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

func scopeRecoveryManifestHash(manifest ScopeRecoveryManifest) string {
	payload, _ := json.Marshal(manifest)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func legacyAssignmentRecoverable(task *model.Task) bool {
	if task == nil {
		return false
	}
	if len(task.AssignedAreas) == 0 {
		return len(task.AssignedAreaKinds) == 0
	}
	if len(task.AssignedAreaKinds) != len(task.AssignedAreas) {
		return false
	}
	for _, area := range task.AssignedAreas {
		if task.AssignedAreaKinds[area] != model.AreaUnknown {
			return false
		}
	}
	return true
}

func activeScopeRecoveryTask(s *model.Snapshot, task *model.Task) bool {
	if task == nil {
		return true
	}
	// RunID and a writing preflight survive orderly stop/restart as continuation
	// history. Only a durable unfinished run, a queued/running native check, or
	// an integration reservation proves activity after the local lock is held.
	for _, run := range s.Runs {
		if run.Task == task.ID && run.Outcome == "running" {
			return true
		}
	}
	for _, check := range s.Capacity.Verification {
		if check.Task == task.ID {
			return true
		}
	}
	if s.IntegrationBatch != nil {
		for _, member := range s.IntegrationBatch.Tasks {
			if member.ID == task.ID {
				return true
			}
		}
	}
	return false
}

func validateRetainedScopeRecoveryWorktree(ctx context.Context, p *Project, task *model.Task, areas []gitx.Area) error {
	path := p.TaskPath(task)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect scope recovery task %s worktree: %w", task.ID, err)
	}
	worktree := gitx.Git{Dir: path}
	head, err := worktree.SHA(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("read scope recovery task %s worktree head: %w", task.ID, err)
	}
	if head != task.HeadSHA {
		return fmt.Errorf("scope recovery task %s worktree head differs from its durable checkpoint", task.ID)
	}
	if err = p.Git.ValidateFullCheckpointScope(ctx, path, task.BaseSHA, areas); err != nil {
		return fmt.Errorf("validate scope recovery task %s retained worktree: %w", task.ID, err)
	}
	return nil
}

func additionalDependencies(s *model.Snapshot, task *model.Task, requested []string) ([]string, error) {
	deps := append([]string(nil), task.Dependencies...)
	seen := map[string]bool{}
	for _, dependency := range deps {
		if dependency == "" || seen[dependency] {
			return nil, errors.New("existing dependencies are invalid")
		}
		seen[dependency] = true
	}
	for _, dependency := range requested {
		if !scopeRecoveryTaskID.MatchString(dependency) || dependency == task.ID || s.Tasks[dependency] == nil || seen[dependency] {
			return nil, errors.New("additional dependencies must name distinct existing predecessor tasks")
		}
		seen[dependency] = true
		deps = append(deps, dependency)
	}
	return deps, nil
}

func validateScopeRecoveryOwnership(ctx context.Context, p *Project, effective config.Effective, s *model.Snapshot, prepared []scopeRecoveryPrepared) error {
	prospective := model.Clone(s)
	for _, item := range prepared {
		task := prospective.Tasks[item.item.ID]
		task.AssignedAreas = canonicalAssignedAreas(item.areas)
		task.AssignedAreaKinds = normalizeAreaKinds(item.areas)
		task.Dependencies = item.dependencies
	}
	for _, item := range prepared {
		task := prospective.Tasks[item.item.ID]
		for _, owner := range prospective.Tasks {
			if owner == nil || owner.ID == task.ID || owner.State == model.Done {
				continue
			}
			ownerAreas, ok := immutableScope(owner)
			if !ok {
				if provablyUnstartedLegacy(owner) {
					var err error
					ownerAreas, err = p.Git.ClassifyAreasAtRef(ctx, effective.BaseSHA, owner.Areas)
					if err != nil {
						return fmt.Errorf("classify unfinished legacy task %s ownership: %w", owner.ID, err)
					}
				} else {
					// A started unknown owner has no trustworthy boundary to compare.
					// Do not silently treat it as disjoint: require this task to wait
					// for the owner, or include that owner in this same reauthorization.
					if !scopeRecoverySerialized(prospective.Tasks, task.ID, owner.ID) {
						return fmt.Errorf("scope recovery task %s has unfinished legacy task %s with unknown ownership and no predecessor dependency", task.ID, owner.ID)
					}
					continue
				}
			}
			if !scopeRecoveryAreasOverlap(item.areas, ownerAreas) {
				continue
			}
			if !scopeRecoverySerialized(prospective.Tasks, task.ID, owner.ID) {
				return fmt.Errorf("scope recovery task %s overlaps unfinished task %s without a predecessor dependency", task.ID, owner.ID)
			}
		}
	}
	if hasDependencyCycle(prospective.Tasks) {
		return errors.New("scope recovery additional dependencies introduce a cycle")
	}
	return nil
}

// scopeRecoverySerialized accepts either dependency direction. If A depends on
// B, B finishes before A; if B already depends on A, A finishes before B. Both
// forms serialize overlapping ownership without inventing a reverse edge that
// would create a cycle in a partially recovered batch.
func scopeRecoverySerialized(tasks map[string]*model.Task, a, b string) bool {
	return taskDependsOn(tasks, a, b) || taskDependsOn(tasks, b, a)
}

func taskDependsOn(tasks map[string]*model.Task, start, target string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if id == target {
			return true
		}
		if seen[id] || tasks[id] == nil {
			return false
		}
		seen[id] = true
		for _, dependency := range tasks[id].Dependencies {
			if visit(dependency) {
				return true
			}
		}
		return false
	}
	return start != target && visit(start)
}

func provablyUnstartedLegacy(task *model.Task) bool {
	return task != nil && task.BaseSHA == "" && task.HeadSHA == "" && (task.State == model.Planned || task.State == model.Ready) && len(task.Areas) != 0
}

func scopeRecoveryAreasOverlap(a, b []gitx.Area) bool {
	for _, left := range a {
		for _, right := range b {
			if gitx.ValidateScopePaths([]gitx.Area{left}, []string{right.Pattern}) == nil || gitx.ValidateScopePaths([]gitx.Area{right}, []string{left.Pattern}) == nil {
				return true
			}
		}
	}
	return false
}

func hasDependencyCycle(tasks map[string]*model.Task) bool {
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if visited[id] || tasks[id] == nil {
			return false
		}
		visiting[id] = true
		for _, dependency := range tasks[id].Dependencies {
			if visit(dependency) {
				return true
			}
		}
		delete(visiting, id)
		visited[id] = true
		return false
	}
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if visit(id) {
			return true
		}
	}
	return false
}

func applyScopeRecovery(s *model.Snapshot, manifest ScopeRecoveryManifest, prepared []scopeRecoveryPrepared) error {
	if s.Applied[manifest.CommandID] {
		return nil
	}
	for _, preparedTask := range prepared {
		task := s.Tasks[preparedTask.item.ID]
		if task == nil || task.BaseSHA != preparedTask.item.BaseSHA || task.HeadSHA != preparedTask.item.HeadSHA || ScopeRecoveryContractHash(task) != preparedTask.item.ContractHash || !legacyAssignmentRecoverable(task) {
			return errors.New("scope recovery task changed after preflight")
		}
		task.AssignedAreas = canonicalAssignedAreas(preparedTask.areas)
		task.AssignedAreaKinds = normalizeAreaKinds(preparedTask.areas)
		task.Dependencies = append([]string(nil), preparedTask.dependencies...)
		// Any old exact-head approval bound to a different task contract is stale.
		// Findings, budgets, source summaries, and retry guards remain evidence.
		task.Preflight = nil
		task.Evidence = nil
		task.ReviewProvenance = nil
		task.VisualRequired = nil
		if task.State != model.Blocked {
			task.State = model.Ready
			task.Blocker = nil
		}
		record := model.ScopeRecovery{CommandID: manifest.CommandID, StateRef: manifest.ExpectedStateRef, PolicyHash: manifest.PolicyHash, BaseSHA: preparedTask.item.BaseSHA, HeadSHA: preparedTask.item.HeadSHA, ContractHash: preparedTask.item.ContractHash, ManifestHash: scopeRecoveryManifestHash(manifest), Areas: append([]string(nil), task.AssignedAreas...), Dependencies: append([]string(nil), preparedTask.item.AdditionalDependencies...), Reason: preparedTask.item.Reason}
		if err := model.RecordScopeRecovery(task, record); err != nil {
			return err
		}
	}
	s.Applied[manifest.CommandID] = true
	return nil
}

// acquireAndRecoverScope combines ordinary fenced lease acquisition with the
// prepared assignment publication. A schema upgrade must never publish a
// lease-only intermediate snapshot that leaves started legacy tasks unknown.
// It intentionally does not hydrate unrelated legacy tasks, preserving every
// other task byte-for-byte.
func (c *Controller) acquireAndRecoverScope(ctx context.Context, expectedRef string, manifest ScopeRecoveryManifest, prepared []scopeRecoveryPrepared) error {
	s, head, err := c.P.Git.Load(ctx)
	if err != nil {
		return err
	}
	if head != expectedRef || s.Project != c.P.Config.Project.ID {
		return errors.New("scope recovery remote state changed before lease acquisition")
	}
	now := c.nowUTC()
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(now) {
		return fmt.Errorf("%w: held by %s until %s", ErrLease, s.Controller.Machine, s.Controller.Expires)
	}
	s.Controller = model.Lease{Machine: c.P.Machine.ID, Owner: c.owner, Epoch: s.Controller.Epoch + 1, Heartbeat: now, Expires: now.Add(c.leaseDuration())}
	if err = applyScopeRecovery(s, manifest, prepared); err != nil {
		return err
	}
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

func (c *Controller) releaseScopeRecovery(ctx context.Context) error {
	return c.save(ctx, func(s *model.Snapshot) error {
		s.Controller.Owner = ""
		s.Controller.Expires = c.nowUTC()
		return nil
	})
}
