package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func TestManyReadyPreflightsAreBoundedWithoutStarvingCoding(t *testing.T) {
	s := model.NewSnapshot("project123")
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("a_ui_%02d", i)
		s.Tasks[id] = &model.Task{ID: id, State: model.Ready, UI: true, Domains: []string{id}}
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("z_code_%02d", i)
		s.Tasks[id] = &model.Task{ID: id, State: model.Ready, Domains: []string{id}}
	}
	active, guided := map[string]bool{}, map[string]bool{}
	selected := selectPreflights(s, active, guided, 2, 2, roles.Builtins())
	if len(selected) != 5 || selected[0].task.ID != "z_code_00" || selected[1].task.ID != "z_code_01" {
		t.Fatalf("bounded preflight did not prioritize independent coding: %#v", selected)
	}
	for _, candidate := range selected {
		active[candidate.task.ID] = false
		guided[candidate.task.ID] = candidate.guided
	}
	if next := selectPreflights(s, active, guided, 2, 2, roles.Builtins()); len(next) != 0 {
		t.Fatalf("reader and fast queue caps were exceeded: %#v", next)
	}
	delete(active, "z_code_00")
	delete(guided, "z_code_00")
	s.Tasks["z_code_00"].Preflight = &model.Preflight{Phase: "ready"}
	selected = selectPreflights(s, active, guided, 2, 2, roles.Builtins())
	if len(selected) != 1 || selected[0].task.ID != "z_code_02" {
		t.Fatalf("released fast slot was not reused deterministically: %#v", selected)
	}
}

func TestPreflightReservationAdmitsIndependentWriters(t *testing.T) {
	s := model.NewSnapshot("project123")
	for _, id := range []string{"ui_a", "ui_b", "ui_c"} {
		s.Tasks[id] = &model.Task{ID: id, State: model.Ready, UI: true, Domains: []string{id}}
	}
	for _, id := range []string{"code_a", "code_b"} {
		s.Tasks[id] = &model.Task{ID: id, State: model.Ready, Domains: []string{id}}
	}
	selected := selectPreflights(s, map[string]bool{}, map[string]bool{}, 2, 2, roles.Builtins())
	code := 0
	for _, candidate := range selected {
		if !candidate.task.UI {
			code++
		}
	}
	if code != 2 {
		t.Fatalf("reader pressure starved independent writers: %#v", selected)
	}
}

func TestAcceptedCorrectionInvalidatesCompletedPreflight(t *testing.T) {
	effective := config.Effective{BaseSHA: "base", Hash: "config", Policy: config.Policy{ImplementationRetries: 2}}
	task := &model.Task{ID: "ui", ObjectiveID: "objective", State: model.Running, HeadSHA: "old-head", UI: true}
	source := &model.Task{ID: "api", ObjectiveID: "objective", HeadSHA: strings.Repeat("a", 40)}
	required := []roles.Role{{Name: "designer", Stage: "pre-implementation"}}
	p := &model.Preflight{Phase: "writing", BaseSHA: "base", HeadSHA: task.HeadSHA, Config: "config", Rules: roles.Hash(), Scope: preflightScope(task, effective), Completed: []string{"designer"}}
	if err := model.QueueGuidance(task, source, "correction", "The durable API uses cursor pagination; update the navigation contract."); err != nil {
		t.Fatal(err)
	}
	if preflightMatches(p, task, effective) {
		t.Fatal("accepted correction retained stale same-head guidance")
	}
	// A failed implementer checkpoints its edits before requesting FIX. It must
	// not inherit advice that predates the accepted correction.
	task.HeadSHA = "new-head"
	task.State = model.Fix
	if reusePreflightForFix(p, task, effective, required) {
		t.Fatal("failed implementation reused guidance predating its correction")
	}
	// Specialist output and checkpoint bookkeeping are not new task inputs.
	p.Scope = preflightScope(task, effective)
	task.Decisions = append(task.Decisions, "designer: use existing navigation", "Checkpoint: repaired navigation")
	if !reusePreflightForFix(p, task, effective, required) {
		t.Fatal("ordinary progress invalidated unchanged task guidance")
	}
}

func TestEligibleOperatorGuidanceInvalidatesPreflightButStalePolicyDoesNot(t *testing.T) {
	head, configHash, rules := strings.Repeat("a", 40), strings.Repeat("b", 64), roles.Hash()
	effective := config.Effective{BaseSHA: head, Hash: configHash}
	task := &model.Task{ID: "ui", State: model.Running, HeadSHA: head}
	baseline := preflightScope(task, effective)
	p := &model.Preflight{Phase: "ready", BaseSHA: head, HeadSHA: head, Config: configHash, Rules: rules, Scope: baseline}
	if err := model.QueueOperatorGuidance(task, "operator", head, head, configHash, rules, "Use the owned endpoint."); err != nil {
		t.Fatal(err)
	}
	if preflightMatches(p, task, effective) {
		t.Fatal("eligible operator guidance did not invalidate completed preflight")
	}
	changed := effective
	changed.Hash = strings.Repeat("c", 64)
	if got := preflightScope(task, changed); got != baseline {
		t.Fatal("stale-policy operator guidance changed preflight scope")
	}
}

func TestCompletedPreflightSurvivesVerificationRecovery(t *testing.T) {
	for _, state := range []model.State{model.Verifying, model.Review, model.SyncRequired, model.Fix} {
		for _, cleanStop := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/clean-stop=%t", state, cleanStop), func(t *testing.T) {
				effective := config.Effective{BaseSHA: strings.Repeat("a", 40), Hash: strings.Repeat("b", 64), Policy: config.Policy{ImplementationRetries: 2}}
				s := model.NewSnapshot("project123")
				task := &model.Task{ID: "ui", State: state, HeadSHA: strings.Repeat("c", 40), UI: true, Attempts: 1}
				task.Preflight = &model.Preflight{Phase: "writing", BaseSHA: effective.BaseSHA, HeadSHA: strings.Repeat("d", 40), Config: effective.Hash, Rules: roles.Hash(), Scope: preflightScope(task, effective), Completed: []string{"designer"}, ReuseCount: 1}
				s.Tasks[task.ID] = task
				if cleanStop {
					resetInterruptedPreflight(task.Preflight)
				}
				data, err := json.Marshal(s)
				if err != nil {
					t.Fatal(err)
				}
				recovered, _, err := model.Decode(data)
				if err != nil {
					t.Fatal(err)
				}
				if err := recoverSnapshot(recovered); err != nil {
					t.Fatal(err)
				}
				task = recovered.Tasks["ui"]
				if state == model.Verifying || state == model.Review {
					if task.State != model.SyncRequired {
						t.Fatal("verification recovery skipped synchronization")
					}
				}
				// Exact-head verification runs again and requests a bounded repair.
				task.State = model.Fix
				required := []roles.Role{{Name: "designer", Stage: "pre-implementation"}}
				if !reusePreflightForFix(task.Preflight, task, effective, required) {
					t.Fatal("restart discarded completed guidance eligibility")
				}
				if task.Preflight.Phase != "ready" || task.Preflight.ReuseCount != 2 || task.Attempts != 1 {
					t.Fatalf("restart changed retry accounting: %+v", task)
				}
				task.Preflight.Phase = "writing"
				task.HeadSHA = strings.Repeat("e", 40)
				if reusablePreflightForFix(task.Preflight, task, effective, required) {
					t.Fatal("restart reset the durable reuse budget")
				}
			})
		}
	}
}

func TestInterruptedPreflightReaderOwnershipIsCleared(t *testing.T) {
	for _, phase := range []string{"queued", "waiting", "running"} {
		p := &model.Preflight{Phase: phase, Completed: []string{"architecture"}}
		resetInterruptedPreflight(p)
		if p.Phase != "queued" || len(p.Completed) != 1 {
			t.Fatalf("interrupted reader was not safely resumable: %+v", p)
		}
	}
}

func TestPreflightIdentityInvalidatesOnBaseHeadAndPolicyChange(t *testing.T) {
	task := &model.Task{HeadSHA: "head"}
	effective := config.Effective{BaseSHA: "base", Hash: "config"}
	p := &model.Preflight{Phase: "ready", BaseSHA: "base", HeadSHA: "head", Config: "config", Rules: roles.Hash()}
	if !preflightMatches(p, task, effective) {
		t.Fatal("matching preflight was not reusable")
	}
	for _, changed := range []struct {
		task      *model.Task
		effective config.Effective
	}{
		{&model.Task{HeadSHA: "head"}, config.Effective{BaseSHA: "new-base", Hash: "config"}},
		{&model.Task{HeadSHA: "new-head"}, effective},
		{task, config.Effective{BaseSHA: "base", Hash: "new-config"}},
	} {
		if preflightMatches(p, changed.task, changed.effective) {
			t.Fatal("stale preflight was reusable")
		}
	}
}

func TestCompletedPreflightReusesOnlyBoundedUnchangedFixScope(t *testing.T) {
	effective := config.Effective{BaseSHA: "base", Hash: "config", Policy: config.Policy{ImplementationRetries: 2}}
	newTask := func() *model.Task {
		return &model.Task{State: model.Fix, HeadSHA: "new-head", Objective: "repair the UI", Acceptance: []string{"works"}, Areas: []string{"ui"}, Domains: []string{"ui"}, UI: true}
	}
	required := []roles.Role{{Name: "designer", Stage: "pre-implementation"}}
	newPreflight := func(task *model.Task) *model.Preflight {
		return &model.Preflight{Phase: "writing", BaseSHA: "base", HeadSHA: "old-head", Config: "config", Rules: roles.Hash(), Scope: preflightScope(task, effective), Completed: []string{"designer"}}
	}
	task := newTask()
	p := newPreflight(task)
	if preflightMatches(p, task, effective) {
		t.Fatal("changed FIX head matched exact preflight identity")
	}
	if !reusablePreflightForFix(p, task, effective, required) {
		t.Fatal("completed unchanged FIX preflight was not reusable")
	}
	if !reusePreflightForFix(p, task, effective, required) {
		t.Fatal("eligible FIX preflight was not promoted directly to writer admission")
	}
	if !preflightMatches(p, task, effective) || p.Phase != "ready" || p.ReuseCount != 1 || p.ReuseReason == "" {
		t.Fatalf("reused preflight did not become exact-head writer-ready evidence: %+v", p)
	}

	for _, changed := range []struct {
		name   string
		update func(*model.Preflight, *model.Task)
		eff    config.Effective
	}{
		{"ready state", func(_ *model.Preflight, task *model.Task) { task.State = model.Ready }, effective},
		{"scope", func(_ *model.Preflight, task *model.Task) { task.Objective = "redesign the UI" }, effective},
		{"base", func(_ *model.Preflight, _ *model.Task) {}, config.Effective{BaseSHA: "new-base", Hash: "config", Policy: effective.Policy}},
		{"policy", func(_ *model.Preflight, _ *model.Task) {}, config.Effective{BaseSHA: "base", Hash: "new-config", Policy: effective.Policy}},
		{"retry bound", func(p *model.Preflight, _ *model.Task) { p.ReuseCount = 2 }, effective},
		{"incomplete", func(p *model.Preflight, _ *model.Task) { p.Completed = nil }, effective},
	} {
		t.Run(changed.name, func(t *testing.T) {
			task := newTask()
			p := newPreflight(task)
			changed.update(p, task)
			if reusablePreflightForFix(p, task, changed.eff, required) {
				t.Fatal("stale or incomplete preflight was reusable")
			}
		})
	}
}

func TestDirectFixWaiverRequiresExactReviewedTextLayoutRepair(t *testing.T) {
	effective := config.Effective{BaseSHA: strings.Repeat("a", 40), Hash: strings.Repeat("b", 64), Files: map[string]string{
		".aih/roles/animation-architecture.yaml": "name: animation-architecture\nextends: reviewer\nstage: review\n",
		".aih/roles/visual-quality.yaml":         "name: visual-quality\nextends: designer\nstage: review\n",
	}}
	required := []roles.Role{{Name: "designer", Stage: "review"}, {Name: "animation-preflight", Stage: "pre-implementation"}}
	task := &model.Task{
		ID: "ui", State: model.Review, HeadSHA: strings.Repeat("c", 40), Objective: "Repair animation architecture caption layout", Acceptance: []string{"caption fits", "animation-architecture review completes"},
		Areas: []string{"ui"}, Domains: []string{"ui"}, Risk: "high", Dependencies: []string{"semantic-engine"}, Roles: []string{"animation-architecture"}, UI: true,
		Findings: []model.Finding{
			{Role: "visual-quality", Severity: "high", Category: "text-layout", Location: "src/caption.tsx:42", Reason: "Caption overlaps the coordinator label at narrow widths.", Resolution: "Wrap the caption in the existing text-fit component."},
			{Role: "visual-quality", Severity: "high", Category: "text-layout", Location: "src/labels.tsx:58", Reason: "Non-breaking-space labels bypass text-fit measurement and overflow.", Resolution: "Replace NBSP labels before the text-fit validation test."},
		},
	}
	task.Evidence = &model.Evidence{Base: effective.BaseSHA, Head: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Checks: []string{"native check passed"}, Reviews: map[string]string{"visual-quality": "two bounded layout defects", "animation-architecture": "completed", "qa": "completed", "security": "completed"}, ReviewRoster: []string{"animation-architecture", "qa", "security", "visual-quality"}}
	p := directFixWaiver(task, effective, required)
	if p == nil || p.DirectFix == nil || p.DirectFix.Role != "designer" || p.Phase != "queued" {
		t.Fatalf("eligible exact-head review did not produce a structured designer waiver: %+v", p)
	}
	task.State = model.Fix
	if !directFixWaiverMatches(p, task, effective) || !preflightRoleSatisfied(p, task, effective, required[0]) {
		t.Fatal("valid direct FIX waiver was not accepted for the built-in designer")
	}
	if preflightRoleSatisfied(p, task, effective, required[1]) {
		t.Fatal("direct FIX waiver completed a custom pre-implementation role")
	}
	vague := model.Clone(&model.Snapshot{Tasks: map[string]*model.Task{"ui": task}}).Tasks["ui"]
	vague.State = model.Review
	vague.Findings = []model.Finding{{Role: "visual-quality", Severity: "high", Category: "text-layout", Location: "src/labels.tsx:58", Reason: "Label looks wrong", Resolution: "Make label better."}}
	if directFixWaiver(vague, effective, required) != nil {
		t.Fatal("vague visual finding bypassed the designer preflight")
	}
	generic := model.Clone(&model.Snapshot{Tasks: map[string]*model.Task{"ui": task}}).Tasks["ui"]
	generic.State = model.Review
	generic.Findings = []model.Finding{{Role: "visual-quality", Severity: "high", Category: "layout", Location: "src/labels.tsx:58", Reason: "Spacing is wrong", Resolution: "Adjust layout"}}
	if directFixWaiver(generic, effective, required) != nil {
		t.Fatal("generic spacing/layout finding bypassed the designer preflight")
	}
	incompleteReview := model.Clone(&model.Snapshot{Tasks: map[string]*model.Task{"ui": task}}).Tasks["ui"]
	incompleteReview.State = model.Review
	delete(incompleteReview.Evidence.Reviews, "qa")
	if directFixWaiver(incompleteReview, effective, required) != nil {
		t.Fatal("missing QA review completion bypassed the designer preflight")
	}
	incompleteReview.State = model.Fix
	if directFixWaiverMatches(p, incompleteReview, effective) {
		t.Fatal("missing QA review completion was accepted on writer-admission replay")
	}
	for _, changed := range []struct {
		name      string
		edit      func(*model.Task)
		effective config.Effective
	}{
		{"head", func(t *model.Task) { t.HeadSHA = strings.Repeat("d", 40) }, effective},
		{"config", func(*model.Task) {}, config.Effective{BaseSHA: effective.BaseSHA, Hash: strings.Repeat("d", 64)}},
		{"rules", func(*model.Task) {}, effective},
		{"finding", func(t *model.Task) { t.Findings[0].Resolution = "Use a different component." }, effective},
		{"security", func(t *model.Task) { t.Security = true }, effective},
		{"schema path sensitivity", func(t *model.Task) { t.Areas = []string{"src/schema/labels.tsx"} }, effective},
		{"dependency path sensitivity", func(t *model.Task) { t.Dependencies = []string{"src/schema-engine"} }, effective},
	} {
		t.Run(changed.name, func(t *testing.T) {
			copy := model.Clone(&model.Snapshot{Tasks: map[string]*model.Task{"ui": task}}).Tasks["ui"]
			changed.edit(copy)
			if changed.name == "rules" {
				p.DirectFix.Rules = strings.Repeat("d", 64)
				defer func() { p.DirectFix.Rules = roles.Hash() }()
			}
			if directFixWaiverMatches(p, copy, changed.effective) {
				t.Fatal("stale or sensitive direct FIX waiver was accepted")
			}
		})
	}
}
