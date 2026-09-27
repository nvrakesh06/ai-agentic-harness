package engine

import (
	"encoding/json"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

func nativeArtifactHandoffController(t *testing.T, fixture *nativeArtifactResolverFixture) *Controller {
	t.Helper()
	state := filepath.Dir(fixture.sealRoot)
	expectedSealRoot := filepath.Join(state, "native-check-artifacts")
	if fixture.sealRoot != expectedSealRoot {
		if err := os.Rename(fixture.sealRoot, expectedSealRoot); err != nil {
			t.Fatal(err)
		}
		fixture.sealRoot = expectedSealRoot
		dir, err := nativeArtifactReceiptDir(fixture.sealRoot, fixture.receipt)
		if err != nil {
			t.Fatal(err)
		}
		fixture.receiptDir = dir
	}
	return &Controller{P: &Project{
		Dir:    state,
		Config: config.Effective{Project: config.Project{ID: fixture.project}},
	}}
}

func TestReviewNativeArtifactInventoryDeliversVerifiedLocalPNGToPeerAndQA(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	inventory, err := nativeArtifactHandoffController(t, fixture).reviewNativeArtifactInventory(fixture.plan, fixture.task, fixture.evidence)
	if err != nil {
		t.Logf("readiness cause: %v", errors.Unwrap(err))
		t.Fatal(err)
	}
	if len(inventory.images) != 1 || !strings.HasPrefix(inventory.images[0].Path, fixture.receiptDir) {
		t.Fatalf("inventory = %#v", inventory)
	}
	for _, peers := range []bool{false, true} {
		payload := reviewEvidencePayloadWithPeers(fixture.evidence, 1, peers) + nativeArtifactReviewPayload(fixture.plan, inventory)
		if !strings.Contains(payload, inventory.images[0].Path) || !strings.Contains(payload, inventory.images[0].SHA256) || !strings.Contains(payload, "untrusted check artifacts") {
			t.Fatalf("review payload omitted verified native image for peers=%t: %s", peers, payload)
		}
	}
	portable, err := json.Marshal(fixture.evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(portable), inventory.images[0].Path) {
		t.Fatalf("portable evidence retained local image path: %s", portable)
	}
}

func TestReviewNativeArtifactInventoryFailsBeforeRoleDispatchForMissingOrChangedInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*nativeArtifactResolverFixture)
	}{
		{
			name: "missing current receipt",
			mutate: func(fixture *nativeArtifactResolverFixture) {
				fixture.evidence.Checks[0] = strings.TrimSuffix(fixture.evidence.Checks[0], " artifact="+fixture.receipt)
			},
		},
		{
			name: "new validation input",
			mutate: func(fixture *nativeArtifactResolverFixture) {
				fixture.plan.Input = strings.Repeat("f", 64)
				fixture.evidence.ValidationInput = fixture.plan.Input
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNativeArtifactResolverFixture(t)
			test.mutate(fixture)
			dispatched := 0
			if _, err := nativeArtifactHandoffController(t, fixture).reviewNativeArtifactInventory(fixture.plan, fixture.task, fixture.evidence); err == nil {
				dispatched++
			} else {
				var readiness *NativeArtifactReadinessError
				if !errors.As(err, &readiness) {
					t.Fatalf("error %T is not a readiness error: %v", err, err)
				}
			}
			if dispatched != 0 {
				t.Fatal("a review role would have been dispatched despite unavailable native artifacts")
			}
		})
	}
}

func TestReviewNativeArtifactInventoryRejectsDuplicateReceiptAcrossChecks(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	fixture.plan.Checks = append(fixture.plan.Checks, fixture.plan.Checks[0])
	fixture.evidence.Checks = append(fixture.evidence.Checks, fixture.evidence.Checks[0])
	if _, err := nativeArtifactHandoffController(t, fixture).reviewNativeArtifactInventory(fixture.plan, fixture.task, fixture.evidence); err == nil {
		t.Fatal("accepted one receipt attached to two opted checks")
	} else {
		var readiness *NativeArtifactReadinessError
		if !errors.As(err, &readiness) {
			t.Fatalf("error %T is not a readiness error: %v", err, err)
		}
	}
}

func TestReviewNativeArtifactInventoryKeepsSameHashImagesFromDifferentChecks(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	state := filepath.Dir(fixture.sealRoot)
	space := filepath.Dir(state)
	secondCheck := config.Check{Name: "browser phase two", Command: []string{"test"}, Artifacts: true}
	second := sealFixturePending(t, space, "second-staging", color.RGBA{R: 255, A: 255}, fixture.plan.ExpectedHead)
	second.check, second.index = secondCheck, 1
	if err := os.Rename(filepath.Join(second.staging, "frame.png"), filepath.Join(second.staging, "phase-two.png")); err != nil {
		t.Fatal(err)
	}
	bind := &nativeArtifactContext{ExpectedHead: fixture.plan.ExpectedHead, Config: fixture.plan.ExpectedConfig, Rules: roles.Hash(), PlanInput: fixture.plan.Input, Toolchain: fixture.plan.Toolchain, Project: fixture.project, Task: fixture.task.ID, StateRoot: state, SourceRoot: filepath.Join(space, "source"), SealRoot: fixture.sealRoot}
	receipt, err := sealNativeArtifacts(second, bind)
	if err != nil {
		t.Fatal(err)
	}
	fixture.plan.Checks = append(fixture.plan.Checks, secondCheck)
	fixture.evidence.Checks = append(fixture.evidence.Checks, passedCheckEvidence(secondCheck, "")+" artifact="+receipt)
	inventory, err := nativeArtifactHandoffController(t, fixture).reviewNativeArtifactInventory(fixture.plan, fixture.task, fixture.evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.images) != 2 || inventory.images[0].SHA256 != inventory.images[1].SHA256 || inventory.images[0].Path == inventory.images[1].Path {
		t.Fatalf("same-hash check inventory was collapsed or invalid: %#v", inventory.images)
	}
	payload := nativeArtifactReviewPayload(fixture.plan, inventory)
	if strings.Count(payload, inventory.images[0].SHA256) != 2 || !strings.Contains(payload, "phase-two.png") {
		t.Fatalf("payload omitted one distinct same-hash capture: %s", payload)
	}
}

func TestReviewNativeArtifactInventoryLeavesLegacyChecksUntouched(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	fixture.plan.Checks[0].Artifacts = false
	fixture.evidence.Checks[0] = "legacy passed check"
	inventory, err := nativeArtifactHandoffController(t, fixture).reviewNativeArtifactInventory(fixture.plan, fixture.task, fixture.evidence)
	if err != nil || len(inventory.images) != 0 || nativeArtifactReviewPayload(fixture.plan, inventory) != "" {
		t.Fatalf("legacy inventory = %#v, %v", inventory, err)
	}
}
