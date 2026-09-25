package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestFocusedPackageOnlyAllowsOneOrdinaryGoPackage(t *testing.T) {
	for _, tc := range []struct {
		name, want, reason string
		paths              []string
	}{
		{"ordinary package", "./internal/demo", "", []string{"internal/demo/demo.go", "internal/demo/demo_test.go"}},
		{"scheduler", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/engine/workflow.go"}},
		{"core cli", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/cli/cli.go"}},
		{"core command", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"cmd/aih/main.go"}},
		{"core git", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/gitx/git.go"}},
		{"core provider", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/provider/provider.go"}},
		{"core roles", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/roles/roles.go"}},
		{"security", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/provider/auth.go"}},
		{"network", "", "scheduler, schema, security, configuration, or toolchain input changed", []string{"internal/provider/network.go"}},
		{"multiple packages", "", "change spans more than one Go package", []string{"internal/demo/demo.go", "internal/other/other.go"}},
		{"cross-package rename", "", "change spans more than one Go package", []string{"internal/demo/old.go", "internal/other/new.go"}},
		{"test input", "", "changed paths are not confined to one Go package", []string{"internal/demo/testdata/case.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := focusedPackage(tc.paths)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("focusedPackage(%v) = (%q, %q), want (%q, %q)", tc.paths, got, reason, tc.want, tc.reason)
			}
		})
	}
}

func TestTaskValidationPlanFallsBackToFullForDeletedFinalPackage(t *testing.T) {
	ctx := context.Background()
	head, err := (gitx.Git{Dir: "."}).SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	projectDir, err := (gitx.Git{Dir: "."}).Run(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	effective := config.Effective{Hash: strings.Repeat("a", 64), Project: config.Project{Checks: []config.Check{{Name: "test", Command: []string{"go", "test", "./..."}, Timeout: 60}}}}
	plan, err := (&Controller{}).taskValidationPlan(ctx, effective, &model.Task{Risk: "low", HeadSHA: head}, projectDir, []string{"internal/deleted/final.go"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Gate != "full" || plan.Reason != "focused package is absent at validation head" {
		t.Fatalf("deleted package plan = %#v", plan)
	}
}

func TestTaskValidationPlanFocusesLowRiskPackageAndKeepsHigherRiskFull(t *testing.T) {
	ctx := context.Background()
	head, err := (gitx.Git{Dir: "."}).SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	projectDir, err := (gitx.Git{Dir: "."}).Run(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	effective := config.Effective{Hash: strings.Repeat("a", 64), Project: config.Project{Checks: []config.Check{
		{Name: "test", Command: []string{"go", "test", "-p=1", "./..."}, Timeout: 60},
		{Name: "vet", Command: []string{"go", "vet", "./..."}, Timeout: 60},
		{Name: "release", Command: []string{"go", "run", "./cmd/release"}, Timeout: 60},
	}}}
	low, err := (&Controller{}).taskValidationPlan(ctx, effective, &model.Task{Risk: "low", HeadSHA: head}, projectDir, []string{"internal/demo/demo.go"})
	if err != nil {
		t.Fatal(err)
	}
	if low.Gate != "focused" || len(low.Checks) != 2 || low.ValidationInputMissing() {
		t.Fatalf("low-risk plan = %#v", low)
	}
	for _, check := range low.Checks {
		if strings.Join(check.Command, " ") == "go run ./cmd/release" || !strings.Contains(strings.Join(check.Command, " "), "./internal/demo") {
			t.Fatalf("unexpected focused check: %v", check.Command)
		}
	}
	high, err := (&Controller{}).taskValidationPlan(ctx, effective, &model.Task{Risk: "high", HeadSHA: head}, projectDir, []string{"internal/demo/demo.go"})
	if err != nil {
		t.Fatal(err)
	}
	if high.Gate != "full" || len(high.Checks) != 3 || high.ValidationInputMissing() {
		t.Fatalf("high-risk plan = %#v", high)
	}
}

func (p validationPlan) ValidationInputMissing() bool {
	return p.Input == "" || p.Toolchain == "" || p.TestInputs == ""
}

func TestApplyValidationEvidenceRefreshesRecoveryGateWithoutReplacingIntegration(t *testing.T) {
	evidence := &model.Evidence{IntegrationSHA: strings.Repeat("a", 40), IntegrationOwner: "controller", ValidationGate: "focused", ValidationReason: "old focused reason", ValidationInput: "old", Toolchain: "old-tool", TestInputs: "old-tree"}
	plan := validationPlan{Gate: "full", Reason: "exact integrated merge-train head", Input: "new", Toolchain: "tool", TestInputs: "tree"}
	if err := applyValidationEvidence(evidence, plan, []string{"check=tests exit=0"}); err != nil {
		t.Fatal(err)
	}
	if evidence.IntegrationSHA != strings.Repeat("a", 40) || evidence.ValidationGate != "full" || evidence.ValidationReason != "exact integrated merge-train head" || evidence.ValidationInput != "new" || evidence.Toolchain != "tool" || evidence.TestInputs != "tree" || len(evidence.Checks) != 1 {
		t.Fatalf("recovery evidence = %#v", evidence)
	}
}

func TestFocusedGoCheckRewritesOnlyGoTestAndVetUniverse(t *testing.T) {
	for _, tc := range []struct {
		command []string
		ok      bool
	}{
		{[]string{"go", "test", "-p=1", "./...", "-count=1"}, true},
		{[]string{"go", "vet", "./..."}, true},
		{[]string{"go", "build", "./..."}, false},
		{[]string{"npm", "test"}, false},
	} {
		check, ok := focusedGoCheck(config.Check{Command: tc.command}, "./internal/demo")
		if ok != tc.ok {
			t.Fatalf("focusedGoCheck(%v) ok=%v, want %v", tc.command, ok, tc.ok)
		}
		if ok {
			found := false
			for _, arg := range check.Command {
				found = found || arg == "./internal/demo"
			}
			if !found {
				t.Fatalf("focused command did not contain package: %v", check.Command)
			}
		}
	}
}
