package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

const replanSchema = 1

// ReplanRequest is a deliberately bounded operator authorization. It carries
// no inferred historical scope: every source revision and the successor's new
// immutable scope are explicit and independently checked by the supervisor.
type ReplanRequest struct {
	Schema        int               `json:"schema_version"`
	CommandID     string            `json:"command_id"`
	Expected      ReplanExpected    `json:"expected"`
	Originals     []ReplanOriginal  `json:"originals"`
	Sources       []ReplanSource    `json:"sources"`
	Replacement   ReplanReplacement `json:"replacement"`
	Reason        string            `json:"reason"`
	CandidateHead string            `json:"candidate_head,omitempty"`
}

type ReplanExpected struct {
	BaseSHA  string `json:"base_sha"`
	Config   string `json:"config"`
	Rules    string `json:"rules"`
	StateRef string `json:"state_ref"`
}
type ReplanOriginal struct {
	TaskID  string      `json:"task_id"`
	State   model.State `json:"state"`
	HeadSHA string      `json:"head_sha"`
}
type ReplanSource struct {
	TaskID  string `json:"task_id"`
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
	Order   int    `json:"order"`
}
type ReplanReplacement struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Objective       string   `json:"objective"`
	Acceptance      []string `json:"acceptance"`
	Areas           []string `json:"areas"`
	Domains         []string `json:"conflict_domains"`
	Roles           []string `json:"roles"`
	Risk            string   `json:"risk"`
	UI              bool     `json:"ui"`
	Security        bool     `json:"security"`
	Dependencies    []string `json:"dependencies,omitempty"`
	ReplaceContract bool     `json:"replace_contract,omitempty"`
}

func DecodeReplanRequest(b []byte) (ReplanRequest, error) {
	var request ReplanRequest
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return request, errors.New("replan request contains trailing JSON")
	}
	return request, validateReplanRequest(request)
}

var replanID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`)
var replanSHA = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var replanHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validateReplanRequest(r ReplanRequest) error {
	if r.Schema != replanSchema || !replanID.MatchString(r.CommandID) || !replanID.MatchString(r.Replacement.ID) ||
		!replanSHA.MatchString(r.Expected.BaseSHA) || !replanHash.MatchString(r.Expected.Config) || !replanHash.MatchString(r.Expected.Rules) || !replanSHA.MatchString(r.Expected.StateRef) ||
		len(r.Originals) == 0 || len(r.Originals) > 8 || len(r.Sources) == 0 || len(r.Sources) > 8 || len(r.Reason) == 0 || len(r.Reason) > 1600 || !utf8.ValidString(r.Reason) || strings.ContainsRune(r.Reason, 0) {
		return errors.New("invalid bounded replan request")
	}
	if r.Replacement.Title == "" || r.Replacement.Objective == "" || len(r.Replacement.Acceptance) == 0 || len(r.Replacement.Acceptance) > 32 || len(r.Replacement.Areas) == 0 || len(r.Replacement.Areas) > 32 || len(r.Replacement.Domains) == 0 || len(r.Replacement.Domains) > 16 || (r.Replacement.Risk != "low" && r.Replacement.Risk != "medium" && r.Replacement.Risk != "high") {
		return errors.New("replacement lacks a bounded task contract")
	}
	seen := map[string]string{}
	for _, old := range r.Originals {
		if !replanID.MatchString(old.TaskID) || (old.HeadSHA != "" && !replanSHA.MatchString(old.HeadSHA)) {
			return errors.New("invalid or duplicate original task")
		}
		if _, duplicate := seen[old.TaskID]; duplicate {
			return errors.New("invalid or duplicate original task")
		}
		seen[old.TaskID] = old.HeadSHA
	}
	last := 0
	sourced := map[string]bool{}
	for _, source := range r.Sources {
		originalHead, ok := seen[source.TaskID]
		if !ok || originalHead == "" || originalHead != source.HeadSHA || sourced[source.TaskID] || !replanSHA.MatchString(source.BaseSHA) || !replanSHA.MatchString(source.HeadSHA) || source.BaseSHA == source.HeadSHA || source.Order <= last {
			return errors.New("invalid ordered source checkpoint")
		}
		sourced[source.TaskID] = true
		last = source.Order
	}
	for id, head := range seen {
		if head != "" && !sourced[id] {
			return errors.New("started original lacks a source checkpoint")
		}
	}
	if r.CandidateHead != "" && !replanSHA.MatchString(r.CandidateHead) {
		return errors.New("invalid candidate head")
	}
	return nil
}

func replanActive(s *model.Snapshot, t *model.Task) bool {
	if t == nil {
		return true
	}
	for _, run := range s.Runs {
		if run.Task == t.ID && run.Outcome == "running" {
			return true
		}
	}
	for _, check := range s.Capacity.Verification {
		if check.Task == t.ID {
			return true
		}
	}
	if s.IntegrationBatch != nil {
		for _, member := range s.IntegrationBatch.Tasks {
			if member.ID == t.ID {
				return true
			}
		}
	}
	return false
}

// replanUnstarted proves that an original has no durable implementation or
// verification checkpoint to transfer. An empty requested head is therefore
// not a wildcard: it is accepted only for this narrow, never-started shape.
func replanUnstarted(s *model.Snapshot, t *model.Task) bool {
	if t == nil || t.BaseSHA != "" || t.HeadSHA != "" || t.MergeSHA != "" || t.PostVerifySHA != "" || t.SyncBase != "" ||
		t.RecoveryRequired || t.Attempts != 0 || t.Rotations != 0 || t.AdvisorUsed || t.RunID != "" || t.Preflight != nil || t.Verification != nil ||
		t.Evidence != nil || t.VisualRequired != nil || t.Blocker != nil || t.PR != 0 || len(t.Findings) != 0 || len(t.Summary) != 0 ||
		len(t.ReportedTests) != 0 || len(t.Risks) != 0 || len(t.Decisions) != 0 || len(t.ReviewProvenance) != 0 {
		return false
	}
	for _, cycles := range t.FixCycles {
		if cycles != 0 {
			return false
		}
	}
	for _, run := range s.Runs {
		if run.Task == t.ID {
			return false
		}
	}
	for _, check := range s.Capacity.Verification {
		if check.Task == t.ID {
			return false
		}
	}
	return true
}

func replanDigest(request ReplanRequest) string {
	encoded, _ := json.Marshal(request)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func replanReceipt(s *model.Snapshot, request ReplanRequest) (bool, error) {
	receipt, ok := s.Replans[request.CommandID]
	if !ok {
		if s.Applied[request.CommandID] {
			return false, errors.New("command ID was already used by another operation")
		}
		return false, nil
	}
	if receipt.Digest != replanDigest(request) || receipt.ReplacementID != request.Replacement.ID {
		return false, errors.New("replan command ID does not match its accepted manifest")
	}
	return true, nil
}

func replanPolicyRequired(s *model.Snapshot, request ReplanRequest) (bool, error) {
	accepted, err := replanReceipt(s, request)
	return !accepted, err
}

func mergeReplanContract(r ReplanRequest, originals []*model.Task) (model.Task, error) {
	originalSet := make(map[string]bool, len(originals))
	for _, old := range originals {
		if old != nil {
			originalSet[old.ID] = true
		}
	}
	for _, dependency := range r.Replacement.Dependencies {
		if originalSet[dependency] {
			return model.Task{}, errors.New("replacement cannot explicitly depend on a superseded original")
		}
	}
	next := model.Task{ID: r.Replacement.ID, Title: r.Replacement.Title, Objective: r.Replacement.Objective, Acceptance: uniqueStrings(r.Replacement.Acceptance), Areas: uniqueStrings(r.Replacement.Areas), Domains: uniqueStrings(r.Replacement.Domains), Roles: uniqueStrings(r.Replacement.Roles), Risk: r.Replacement.Risk, UI: r.Replacement.UI, Security: r.Replacement.Security, Dependencies: uniqueStrings(r.Replacement.Dependencies), State: model.Ready, Branch: "aih/" + r.Replacement.ID, FixCycles: map[string]int{}}
	if len(next.Acceptance) != len(r.Replacement.Acceptance) || len(next.Areas) != len(r.Replacement.Areas) || len(next.Domains) != len(r.Replacement.Domains) {
		return next, errors.New("replacement has duplicate contract entries")
	}
	for _, old := range originals {
		// A known immutable boundary is part of the original authorization and
		// cannot be silently narrowed by the replacement contract. An unstarted
		// legacy placeholder with no classified assignment has no trustworthy
		// source boundary; its explicit replacement contract supplies one instead.
		if areas, ok := replanTransferredAreas(r, old); ok {
			next.Areas = unionStrings(next.Areas, areas)
		}
		if !r.Replacement.ReplaceContract && old.Objective != next.Objective {
			return next, errors.New("replacement changes an original contract without replace_contract")
		}
		next.Acceptance = unionStrings(next.Acceptance, old.Acceptance)
		next.Roles = unionStrings(next.Roles, old.Roles)
		for _, dependency := range old.Dependencies {
			if !originalSet[dependency] {
				next.Dependencies = unionStrings(next.Dependencies, []string{dependency})
			}
		}
		if riskRank(old.Risk) > riskRank(next.Risk) {
			next.Risk = old.Risk
		}
		next.UI = next.UI || old.UI
		next.Security = next.Security || old.Security
	}
	return next, nil
}

func replanTransferredAreas(request ReplanRequest, task *model.Task) ([]string, bool) {
	areas, ok := model.ImmutableAreas(task)
	if !ok {
		return nil, false
	}
	if len(task.AssignedAreas) != 0 {
		for _, kind := range model.ImmutableAreaKinds(task) {
			if kind != model.AreaUnknown {
				continue
			}
			// A started task's historical assignment remains an authorization
			// boundary even if a legacy runtime could not classify its kind.
			if !replanRequestMarksUnstarted(request, task.ID) {
				return areas, true
			}
			return nil, false
		}
		return areas, true
	}
	// The only assignment-free fallback is an explicitly unstarted original.
	// Its mutable legacy Areas prose must not be promoted into the successor.
	if replanRequestMarksUnstarted(request, task.ID) {
		return nil, false
	}
	return areas, true
}

func replanRequestMarksUnstarted(request ReplanRequest, id string) bool {
	for _, original := range request.Originals {
		if original.TaskID == id {
			return original.HeadSHA == ""
		}
	}
	return false
}

func riskRank(value string) int {
	if value == "high" {
		return 3
	}
	if value == "medium" {
		return 2
	}
	return 1
}
func uniqueStrings(values []string) []string { return unionStrings(nil, values) }
func unionStrings(left, right []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range append(left, right...) {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// applyReplan creates the successor source commit first, then publishes that
// new ref and the replacement links in one existing fenced state transaction.
// A conflict leaves neither a state change nor a published replacement branch.
func (c *Controller) applyReplan(ctx context.Context, request ReplanRequest) error {
	if err := validateReplanRequest(request); err != nil {
		return err
	}
	s := c.Snapshot()
	if applied, receiptErr := replanReceipt(s, request); receiptErr != nil {
		return receiptErr
	} else if applied {
		return c.reconcileReplanIssue(ctx, request.Replacement.ID)
	}
	effective, err := c.effective(ctx)
	if err != nil {
		return err
	}
	if request.Expected.BaseSHA != effective.BaseSHA || request.Expected.Config != effective.Hash || request.Expected.Rules != roles.Hash() {
		return errors.New("replan policy or canonical main changed")
	}
	originals := make([]*model.Task, 0, len(request.Originals))
	objective := ""
	oldSet := map[string]bool{}
	for _, expected := range request.Originals {
		t := s.Tasks[expected.TaskID]
		if t == nil || t.State != expected.State || t.State == model.Done || t.State == model.Superseded || t.MergeSHA != "" || replanActive(s, t) {
			return fmt.Errorf("original task %s is not idle at its expected checkpoint", expected.TaskID)
		}
		if expected.HeadSHA == "" {
			if !replanUnstarted(s, t) {
				return fmt.Errorf("original task %s is not provably unstarted", expected.TaskID)
			}
			if t.Branch != "" {
				if remote, e := c.P.Git.RemoteHead(ctx, t.Branch); e != nil || remote != "" {
					return fmt.Errorf("unstarted original task %s source ref exists or cannot be checked", t.ID)
				}
			}
		} else if t.HeadSHA != expected.HeadSHA {
			return fmt.Errorf("original task %s is not idle at its expected checkpoint", expected.TaskID)
		}
		if objective == "" {
			objective = t.ObjectiveID
		} else if objective != t.ObjectiveID {
			return errors.New("replan originals must share one objective")
		}
		if expected.HeadSHA != "" {
			remote, e := c.P.Git.RemoteHead(ctx, t.Branch)
			if e != nil || remote != t.HeadSHA {
				return fmt.Errorf("original task %s source ref changed", t.ID)
			}
		}
		originals, oldSet[t.ID] = append(originals, t), true
	}
	if objective == "" || s.Tasks[request.Replacement.ID] != nil {
		return errors.New("replacement task already exists or originals lack an objective")
	}
	next, err := mergeReplanContract(request, originals)
	if err != nil {
		return err
	}
	configuredRoles, err := roles.Load(effective.Files)
	if err != nil {
		return err
	}
	for _, role := range next.Roles {
		if _, ok := configuredRoles[role]; !ok {
			return fmt.Errorf("replacement has unknown role %s", role)
		}
	}
	next.ObjectiveID = objective
	next.Replan = &model.ReplanProvenance{CommandID: request.CommandID, Reason: request.Reason, CandidateHead: request.CandidateHead}
	for _, source := range request.Sources {
		next.Replan.Sources = append(next.Replan.Sources, model.ReplanCheckpoint{TaskID: source.TaskID, BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA, Order: source.Order})
	}
	for _, dep := range next.Dependencies {
		if oldSet[dep] || s.Tasks[dep] == nil {
			return errors.New("replacement has invalid dependency")
		}
	}
	if prospectiveReplanCycle(s, &next, request.Originals) {
		return errors.New("replacement dependencies form a cycle")
	}
	for _, member := range request.Sources {
		t := s.Tasks[member.TaskID]
		if t == nil || t.BaseSHA != member.BaseSHA || t.HeadSHA != member.HeadSHA || !c.P.Git.Ancestor(ctx, member.BaseSHA, member.HeadSHA) {
			return fmt.Errorf("source checkpoint %s changed or is invalid", member.TaskID)
		}
	}
	// Ownership is evaluated after replacement links exist. An overlapping
	// unfinished task is safe only when the prospective graph proves that one
	// task waits for the other; a superseded dependency resolves to its successor.
	prospective := prospectiveReplanSnapshot(s, &next, request.Originals)
	for _, task := range prospective.Tasks {
		if oldSet[task.ID] || task.ID == next.ID || task.State == model.Done || task.State == model.Superseded {
			continue
		}
		areas, known := replanOwnershipAreas(task)
		if !known {
			return fmt.Errorf("replacement cannot prove non-overlap with unfinished legacy task %s", task.ID)
		}
		if areasOverlap(next.Areas, areas) || stringsOverlap(next.Domains, task.Domains) {
			if replanSerialized(prospective.Tasks, next.ID, task.ID) {
				continue
			}
			return fmt.Errorf("replacement overlaps unfinished task %s", task.ID)
		}
	}
	areas, err := c.P.Git.ClassifyAreasAtRef(ctx, effective.BaseSHA, next.Areas)
	if err != nil {
		return err
	}
	next.AssignedAreas, next.AssignedAreaKinds = canonicalAssignedAreas(areas), normalizeAreaKinds(areas)
	var head string
	if request.CandidateHead != "" {
		head, err = c.P.Git.SHA(ctx, request.CandidateHead)
		if err == nil && !c.P.Git.Ancestor(ctx, effective.BaseSHA, head) {
			err = errors.New("candidate does not descend from canonical main")
		}
		for _, source := range request.Sources {
			if err == nil && !c.P.Git.Ancestor(ctx, source.HeadSHA, head) {
				err = fmt.Errorf("candidate omits source checkpoint %s", source.TaskID)
			}
		}
	} else {
		sources := make([]gitx.ReplanCheckpoint, 0, len(request.Sources))
		for _, source := range request.Sources {
			sources = append(sources, gitx.ReplanCheckpoint{BaseSHA: source.BaseSHA, HeadSHA: source.HeadSHA})
		}
		head, err = c.P.Git.ReplanBranch(ctx, effective.BaseSHA, sources, "AIH replan "+next.ID)
	}
	if err != nil {
		return err
	}
	if err = c.P.Git.ValidateCommitScope(ctx, effective.BaseSHA, head, areas); err != nil {
		return err
	}
	next.BaseSHA, next.HeadSHA = effective.BaseSHA, head
	if remote, e := c.P.Git.RemoteHead(ctx, "main"); e != nil || remote != effective.BaseSHA {
		return errors.New("canonical main changed during replan validation")
	}
	for _, source := range request.Sources {
		t := s.Tasks[source.TaskID]
		if remote, e := c.P.Git.RemoteHead(ctx, t.Branch); e != nil || remote != source.HeadSHA {
			return fmt.Errorf("source checkpoint %s changed during replan validation", source.TaskID)
		}
	}
	if err := c.save(ctx, func(current *model.Snapshot) error {
		if applied, err := replanReceipt(current, request); err != nil {
			return err
		} else if applied {
			return nil
		}
		if current.Tasks[next.ID] != nil {
			return errors.New("replacement task appeared during replan")
		}
		for _, expected := range request.Originals {
			t := current.Tasks[expected.TaskID]
			if t == nil || t.State != expected.State || replanActive(current, t) || (expected.HeadSHA == "" && !replanUnstarted(current, t)) || (expected.HeadSHA != "" && t.HeadSHA != expected.HeadSHA) {
				return fmt.Errorf("original task %s changed during replan", expected.TaskID)
			}
		}
		current.Tasks[next.ID] = &next
		for _, expected := range request.Originals {
			old := current.Tasks[expected.TaskID]
			old.State = model.Superseded
			old.SupersededBy = next.ID
		}
		current.Applied[request.CommandID] = true
		current.Replans[request.CommandID] = model.ReplanReceipt{Digest: replanDigest(request), ReplacementID: next.ID}
		return nil
	}, gitx.Update{Branch: next.Branch, Old: "", New: head}); err != nil {
		return err
	}
	return c.reconcileReplanIssue(ctx, next.ID)
}

func (c *Controller) reconcileReplanIssue(ctx context.Context, id string) error {
	t := c.Snapshot().Tasks[id]
	if t == nil {
		return errors.New("replan successor is missing")
	}
	if t.Issue == 0 {
		issue, err := c.P.Hub.EnsureIssue(ctx, t.ID, t.Title, c.issueBody(t))
		if err != nil {
			return err
		}
		if err = c.save(ctx, func(s *model.Snapshot) error {
			if s.Tasks[id] == nil || s.Tasks[id].Issue != 0 {
				return errors.New("replan successor changed during issue reconciliation")
			}
			s.Tasks[id].Issue = issue
			return nil
		}); err != nil {
			return err
		}
	}
	c.mirrorWith(ctx, id)
	return nil
}

// Replan is intentionally a one-shot supervisor operation. It acquires the
// ordinary remote lease but never runs recovery, scheduling, providers, or
// checks; after the single fenced publication it releases that lease again.
func Replan(ctx context.Context, project *Project, request ReplanRequest) (err error) {
	if err = validateReplanRequest(request); err != nil {
		return err
	}
	lock, err := platform.Acquire(projectSupervisorLock(project))
	if err != nil {
		return err
	}
	defer lock.Close()
	// Validate the operator's exact snapshot and canonical policy before this
	// one-shot operation publishes even its lease. Unlike Serve, this path never
	// calls recovery or scheduler admission while holding that lease.
	before, stateRef, err := project.Git.Load(ctx)
	if err != nil {
		return err
	}
	if err = replanSnapshotPrecondition(before, stateRef, request); err != nil {
		return err
	}
	policyRequired, err := replanPolicyRequired(before, request)
	if err != nil {
		return err
	}
	if policyRequired {
		if err = replanUnstartedRefs(ctx, project.Git, before, request); err != nil {
			return err
		}
		effective, err := Canonical(ctx, project.Git)
		if err != nil || effective.BaseSHA != request.Expected.BaseSHA || effective.Hash != request.Expected.Config || roles.Hash() != request.Expected.Rules {
			if err != nil {
				return err
			}
			return errors.New("replan policy or canonical main changed")
		}
	}
	c := New(project)
	expectedStateRef := ""
	if policyRequired {
		expectedStateRef = stateRef
	}
	if err = c.acquireReplan(ctx, expectedStateRef); err != nil {
		return err
	}
	defer func() {
		releaseErr := c.save(context.Background(), func(s *model.Snapshot) error {
			s.Controller.Owner = ""
			s.Controller.Expires = c.nowUTC()
			return nil
		})
		if err == nil && releaseErr != nil {
			err = releaseErr
		}
	}()
	return c.applyReplan(ctx, request)
}

// replanUnstartedRefs keeps an empty task checkpoint fail-closed. A declared
// branch for a never-started task must not have appeared remotely between plan
// admission and this bounded replacement request.
func replanUnstartedRefs(ctx context.Context, g gitx.Git, s *model.Snapshot, request ReplanRequest) error {
	for _, expected := range request.Originals {
		if expected.HeadSHA != "" {
			continue
		}
		t := s.Tasks[expected.TaskID]
		if t != nil && t.Branch != "" {
			if remote, err := g.RemoteHead(ctx, t.Branch); err != nil || remote != "" {
				return fmt.Errorf("unstarted original task %s source ref exists or cannot be checked", expected.TaskID)
			}
		}
	}
	return nil
}

func replanSnapshotPrecondition(s *model.Snapshot, stateRef string, request ReplanRequest) error {
	if s == nil {
		return errors.New("replan snapshot unavailable")
	}
	if applied, err := replanReceipt(s, request); err != nil {
		return err
	} else if applied {
		return nil
	}
	if request.Expected.StateRef != stateRef {
		return errors.New("replan state changed")
	}
	objective := ""
	for _, expected := range request.Originals {
		t := s.Tasks[expected.TaskID]
		if t == nil || t.State != expected.State || replanActive(s, t) || t.MergeSHA != "" || t.State == model.Done || t.State == model.Superseded || (expected.HeadSHA == "" && !replanUnstarted(s, t)) || (expected.HeadSHA != "" && t.HeadSHA != expected.HeadSHA) {
			return fmt.Errorf("original task %s is not idle at its expected checkpoint", expected.TaskID)
		}
		if objective == "" {
			objective = t.ObjectiveID
		} else if objective != t.ObjectiveID {
			return errors.New("replan originals must share one objective")
		}
	}
	return nil
}

// acquireReplan deliberately differs from acquire: it cannot hydrate legacy
// tasks, alter capacity, or run any recovery work before the exact replan
// request has been checked. Its only mutation is the normal fenced lease.
func (c *Controller) acquireReplan(ctx context.Context, expectedStateRef string) error {
	s, head, err := c.P.Git.Load(ctx)
	if err != nil {
		return err
	}
	if s.Project != c.P.Config.Project.ID {
		return errors.New("remote project identity mismatch")
	}
	if expectedStateRef != "" && head != expectedStateRef {
		return errors.New("replan state changed before lease acquisition")
	}
	now := c.nowUTC()
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(now) {
		return fmt.Errorf("%w: held by %s until %s", ErrLease, s.Controller.Machine, s.Controller.Expires)
	}
	s.Controller = model.Lease{Machine: c.P.Machine.ID, Owner: c.owner, Epoch: s.Controller.Epoch + 1, Heartbeat: now, Expires: now.Add(c.leaseDuration())}
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
	if err = c.P.DB.Save(next, s); err != nil {
		return err
	}
	return c.P.DB.Set(LocalLeaseHeartbeatKey, now.Format(time.RFC3339Nano))
}

func projectSupervisorLock(project *Project) string {
	return project.Dir + "/supervisor.lock"
}

func prospectiveReplanCycle(s *model.Snapshot, successor *model.Task, originals []ReplanOriginal) bool {
	probe := prospectiveReplanSnapshot(s, successor, originals)
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if done[id] {
			return false
		}
		task := probe.Tasks[id]
		if task == nil {
			return false
		}
		visiting[id] = true
		edges := append([]string(nil), task.Dependencies...)
		if task.State == model.Superseded {
			edges = append(edges, task.SupersededBy)
		}
		for _, edge := range edges {
			if visit(edge) {
				return true
			}
		}
		delete(visiting, id)
		done[id] = true
		return false
	}
	for id := range probe.Tasks {
		if visit(id) {
			return true
		}
	}
	return false
}

func prospectiveReplanSnapshot(s *model.Snapshot, successor *model.Task, originals []ReplanOriginal) *model.Snapshot {
	probe := model.Clone(s)
	probe.Tasks[successor.ID] = successor
	for _, original := range originals {
		if task := probe.Tasks[original.TaskID]; task != nil {
			task.State = model.Superseded
			task.SupersededBy = successor.ID
		}
	}
	return probe
}

// replanOwnershipAreas accepts durable plan ownership and the narrow unstarted
// fallback. A started task with unknown assigned-area kinds remains fail-closed:
// dependency ordering cannot make an unprovable boundary safe.
func replanOwnershipAreas(task *model.Task) ([]string, bool) {
	areas, known := model.ImmutableAreas(task)
	if !known {
		return nil, false
	}
	if len(task.AssignedAreas) != 0 {
		for _, kind := range model.ImmutableAreaKinds(task) {
			if kind == model.AreaUnknown {
				return nil, false
			}
		}
	}
	return areas, true
}

// replanSerialized accepts either dependency direction. The search follows a
// superseded original through SupersededBy, so a dependent task that still names
// its historical predecessor waits for the replacement rather than running in
// parallel with it.
func replanSerialized(tasks map[string]*model.Task, left, right string) bool {
	return replanTaskDependsOn(tasks, left, right) || replanTaskDependsOn(tasks, right, left)
}

func replanTaskDependsOn(tasks map[string]*model.Task, start, target string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if id == target {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		task := tasks[id]
		if task == nil {
			return false
		}
		if task.State == model.Superseded {
			return visit(task.SupersededBy)
		}
		for _, dependency := range task.Dependencies {
			if visit(dependency) {
				return true
			}
		}
		return false
	}
	return start != target && visit(start)
}

func createsDependencyCycle(s *model.Snapshot, candidate string, dependencies []string) bool {
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if id == candidate {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		t := s.Tasks[id]
		if t == nil {
			return false
		}
		for _, dep := range t.Dependencies {
			if visit(dep) {
				return true
			}
		}
		return false
	}
	for _, dep := range dependencies {
		if visit(dep) {
			return true
		}
	}
	return false
}
func areasOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	return false
}
func stringsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
