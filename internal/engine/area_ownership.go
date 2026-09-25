package engine

import (
	"errors"
	"sort"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

// immutableScope turns the durable planner classification into the Git gate.
// Unknown legacy classifications intentionally have no scope: started legacy
// work must be replanned instead of guessed from a possibly widened Areas list.
func immutableScope(task *model.Task) ([]gitx.Area, bool) {
	areas, ok := model.ImmutableAreas(task)
	if !ok || len(areas) == 0 {
		return nil, false
	}
	kinds := model.ImmutableAreaKinds(task)
	if len(kinds) != len(areas) {
		return nil, false
	}
	out := make([]gitx.Area, 0, len(areas))
	for _, pattern := range areas {
		kind := kinds[pattern]
		if kind == model.AreaUnknown || kind == "" {
			return nil, false
		}
		gitKind := gitx.AreaTrackedFile
		if kind == model.AreaDirectory {
			gitKind = gitx.AreaDirectory
		} else if kind != model.AreaFile {
			return nil, false
		}
		out = append(out, gitx.Area{Pattern: pattern, Kind: gitKind})
	}
	return out, true
}

func (c *Controller) validateTaskScope(task *model.Task) error {
	if task == nil || task.BaseSHA == "" || task.HeadSHA == "" {
		return &gitx.ScopeError{}
	}
	areas, ok := immutableScope(task)
	if !ok {
		return &gitx.ScopeError{}
	}
	return c.P.Git.ValidateCommitScope(c.ctx, task.BaseSHA, task.HeadSHA, areas)
}

type scopeOwner struct {
	id        string
	ambiguous bool
}

func findingInTaskScope(task *model.Task, finding model.Finding) bool {
	path := followupSourceFile(finding.Location)
	areas, ok := immutableScope(task)
	return ok && path != "" && gitx.ValidateScopePaths(areas, []string{path}) == nil
}

func writableOwner(state model.State) bool {
	// A RUNNING writer has already captured its task input. Routing a new
	// finding to it without a durable replay protocol could let it checkpoint
	// and clear the finding before it observes it. Treat it as unavailable;
	// the origin fails closed and a later bounded task can be replanned.
	return state == model.Ready || state == model.Fix
}

func findingScopeOwner(tasks map[string]*model.Task, origin *model.Task, finding model.Finding) scopeOwner {
	if origin == nil || findingInTaskScope(origin, finding) {
		return scopeOwner{}
	}
	path := followupSourceFile(finding.Location)
	if path == "" {
		return scopeOwner{}
	}
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var owner scopeOwner
	for _, id := range ids {
		t := tasks[id]
		if t == nil || t.ID == origin.ID || !writableOwner(t.State) || !findingInTaskScope(t, finding) {
			continue
		}
		if owner.id != "" {
			return scopeOwner{ambiguous: true}
		}
		owner.id = id
	}
	return owner
}

type crossTaskRoute struct {
	local      []model.Finding
	gated      bool
	unresolved bool
}

func (c *Controller) routeCrossTaskFindings(originID string, findings []model.Finding, blocks func(model.Finding) bool) (crossTaskRoute, error) {
	var route crossTaskRoute
	var owners map[string][]model.Finding
	err := c.mutate(func(s *model.Snapshot) error {
		route, owners = applyCrossTaskFindings(s, originID, findings, blocks)
		return nil
	})
	if err != nil {
		return route, err
	}
	for id, routed := range owners {
		if err := c.reviewFollowups(c.Snapshot().Tasks[id], routed); err != nil {
			return route, err
		}
		c.mirror(id)
	}
	return route, nil
}

func applyCrossTaskFindings(s *model.Snapshot, originID string, findings []model.Finding, blocks func(model.Finding) bool) (crossTaskRoute, map[string][]model.Finding) {
	route, owners := crossTaskRoute{}, map[string][]model.Finding{}
	origin := s.Tasks[originID]
	for _, finding := range findings {
		owner := findingScopeOwner(s.Tasks, origin, finding)
		if owner.id == "" || owner.ambiguous || introducesDependencyCycle(s.Tasks, originID, owner.id) { // only blocking, out-of-scope findings may gate the origin
			route.local = append(route.local, finding)
			if blocks(finding) && !findingInTaskScope(origin, finding) {
				route.gated = true
				route.unresolved = true
			}
			continue
		}
		owners[owner.id] = append(owners[owner.id], finding)
		if blocks(finding) {
			route.gated = true
			if !containsTask(origin.Dependencies, owner.id) {
				origin.Dependencies = append(origin.Dependencies, owner.id)
			}
		}
	}
	for id, routed := range owners {
		task := s.Tasks[id]
		task.Findings = appendUniqueFindings(task.Findings, routed)
		task.Decisions = append(task.Decisions, "Cross-task review finding routed from "+originID+".")
	}
	if route.unresolved {
		model.Block(origin, "A blocking review finding is outside this task's immutable areas and has no safe writable owner. Replan a bounded follow-up, then answer to reverify.", "AIH refused to expand task scope or introduce a dependency cycle.", model.SyncRequired)
	} else if route.gated {
		// SyncRequired is scheduler-owned wait state. Unlike BLOCKED_HUMAN it
		// resumes automatically once the routed owner's new dependency is Done.
		origin.State = model.SyncRequired
		origin.Blocker = nil
		origin.Updated = time.Now().UTC()
	}
	return route, owners
}

func introducesDependencyCycle(tasks map[string]*model.Task, origin, owner string) bool {
	if owner == "" {
		return false
	}
	seen := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if id == origin {
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
	return visit(owner)
}

func containsTask(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func findingBlocks(required []roles.Role, finding model.Finding) bool {
	for _, role := range required {
		if role.Name == finding.Role && roles.Blocking(role, []model.Finding{finding}) {
			return true
		}
	}
	return false
}

func scopeError(err error) bool { var target *gitx.ScopeError; return errors.As(err, &target) }

func normalizeAreaKinds(classified []gitx.Area) map[string]string {
	out := make(map[string]string, len(classified))
	for _, area := range classified {
		if area.Kind == gitx.AreaDirectory {
			out[area.Pattern] = model.AreaDirectory
		} else {
			out[area.Pattern] = model.AreaFile
		}
	}
	return out
}

func canonicalAssignedAreas(classified []gitx.Area) []string {
	out := make([]string, 0, len(classified))
	for _, area := range classified {
		out = append(out, area.Pattern)
	}
	return out
}
