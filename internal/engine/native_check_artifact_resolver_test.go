package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestResolveNativeArtifactInventoryReturnsOnlyValidatedLocalPaths(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	inventory, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt)
	if err != nil || inventory == nil || inventory.Receipt != fixture.receipt || len(inventory.Images) != 1 {
		t.Fatalf("resolve inventory = %#v, %v", inventory, err)
	}
	image := inventory.Images[0]
	if !filepath.IsAbs(image.Path) || image.SHA256 == "" || image.Bytes < 1 || image.Width != 1 || image.Height != 1 {
		t.Fatalf("invalid local inventory image: %#v", image)
	}
	if fixture.evidence.Visual != nil {
		t.Fatal("native resolver altered visual approval evidence")
	}
}

func TestResolveNativeArtifactInventoryRejectsCausalManifestMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*nativeArtifactManifest, *nativeArtifactResolverFixture)
	}{
		{"config", func(m *nativeArtifactManifest, _ *nativeArtifactResolverFixture) {
			m.Expected.Config = strings.Repeat("f", 64)
		}},
		{"expected head", func(m *nativeArtifactManifest, _ *nativeArtifactResolverFixture) {
			m.Expected.Head = strings.Repeat("f", 40)
		}},
		{"plan input", func(m *nativeArtifactManifest, _ *nativeArtifactResolverFixture) {
			m.Expected.PlanInput = strings.Repeat("f", 64)
		}},
		{"unsupported version", func(m *nativeArtifactManifest, _ *nativeArtifactResolverFixture) { m.Version++ }},
		{"check index", func(m *nativeArtifactManifest, f *nativeArtifactResolverFixture) {
			m.Check = 1
			f.plan.Checks = append(f.plan.Checks, config.Check{Name: "not opted", Command: []string{"test"}})
		}},
		{"traversal", func(m *nativeArtifactManifest, _ *nativeArtifactResolverFixture) { m.Images[0].Path = "../escape.png" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNativeArtifactResolverFixture(t)
			fixture.receipt = rewriteNativeArtifactManifest(t, fixture.receiptDir, test.mutate, fixture)
			fixture.evidence.Checks[0] = passedCheckEvidence(fixture.plan.Checks[0], "") + " artifact=" + fixture.receipt
			if inventory, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt); err == nil || inventory != nil {
				t.Fatalf("mismatched %s accepted: %#v, %v", test.name, inventory, err)
			}
		})
	}
}

func TestResolveNativeArtifactInventoryRejectsCorruptManifestAndImage(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	manifestPath := filepath.Join(fixture.receiptDir, "manifest.json")
	if err := os.Chmod(manifestPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt); err == nil {
		t.Fatal("corrupt manifest accepted")
	}

	fixture = newNativeArtifactResolverFixture(t)
	imagePath := filepath.Join(fixture.receiptDir, "frame.png")
	if err := os.Chmod(imagePath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt); err == nil {
		t.Fatal("corrupt image accepted")
	}
}

func TestResolveNativeArtifactInventoryRejectsLinksAndReportsMissingAttachment(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	imagePath := filepath.Join(fixture.receiptDir, "frame.png")
	target := filepath.Join(filepath.Dir(fixture.receiptDir), "target.png")
	if err := os.WriteFile(target, []byte("not a png"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(imagePath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, imagePath); err != nil {
		t.Skipf("links unavailable: %v", err)
	}
	if _, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt); err == nil {
		t.Fatal("linked image accepted")
	}

	fixture = newNativeArtifactResolverFixture(t)
	if _, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, filepath.Join(fixture.sealRoot, "absent"), fixture.receipt); !errors.Is(err, ErrNativeArtifactUnavailable) {
		t.Fatalf("missing attachment = %v, want unavailable", err)
	}
}

func TestResolveNativeArtifactInventoryReportsLegacyChecksUnavailable(t *testing.T) {
	fixture := newNativeArtifactResolverFixture(t)
	fixture.evidence.Checks = []string{passedCheckEvidence(fixture.plan.Checks[0], "")}
	if inventory, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt); inventory != nil || !errors.Is(err, ErrNativeArtifactUnavailable) {
		t.Fatalf("legacy check availability = %#v, %v", inventory, err)
	}
	fixture = newNativeArtifactResolverFixture(t)
	fixture.plan.Checks[0].Artifacts = false
	if _, err := ResolveNativeArtifactInventory(fixture.project, fixture.plan, fixture.task, fixture.evidence, fixture.sealRoot, fixture.receipt); err == nil {
		t.Fatal("receipt accepted after check opt-in was removed")
	}
}

type nativeArtifactResolverFixture struct {
	project, sealRoot, receipt, receiptDir string
	plan                                   validationPlan
	task                                   *model.Task
	evidence                               *model.Evidence
}

func newNativeArtifactResolverFixture(t *testing.T) *nativeArtifactResolverFixture {
	t.Helper()
	space := t.TempDir()
	source, state := filepath.Join(space, "source"), filepath.Join(space, "state")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	check := config.Check{Name: "browser", Command: []string{"test"}, Artifacts: true}
	bind := &nativeArtifactContext{ExpectedHead: strings.Repeat("a", 40), Config: strings.Repeat("c", 64), Rules: strings.Repeat("d", 64), PlanInput: strings.Repeat("e", 64), Toolchain: "test=hash", Project: "project", Task: "task", StateRoot: state, SourceRoot: source, SealRoot: filepath.Join(state, "seal")}
	pending := sealFixturePending(t, space, "staging", color.RGBA{R: 255, A: 255}, bind.ExpectedHead)
	pending.check = check
	receipt, err := sealNativeArtifacts(pending, bind)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := nativeArtifactReceiptDir(bind.SealRoot, receipt)
	if err != nil {
		t.Fatal(err)
	}
	plan := validationPlan{ExpectedHead: bind.ExpectedHead, ExpectedConfig: bind.Config, Input: bind.PlanInput, Toolchain: bind.Toolchain, Checks: []config.Check{check}}
	evidence := &model.Evidence{Config: bind.Config, Rules: bind.Rules, ValidationInput: bind.PlanInput, Toolchain: bind.Toolchain, Checks: []string{passedCheckEvidence(check, "") + " artifact=" + receipt}}
	return &nativeArtifactResolverFixture{project: bind.Project, sealRoot: bind.SealRoot, receipt: receipt, receiptDir: dir, plan: plan, task: &model.Task{ID: bind.Task}, evidence: evidence}
}

func rewriteNativeArtifactManifest(t *testing.T, dir string, mutate func(*nativeArtifactManifest, *nativeArtifactResolverFixture), fixture *nativeArtifactResolverFixture) string {
	t.Helper()
	path := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest nativeArtifactManifest
	if err = json.Unmarshal(bytes.TrimSpace(data), &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(&manifest, fixture)
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(payload, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(payload)
	return manifest.ID + "." + hex.EncodeToString(hash[:])
}
