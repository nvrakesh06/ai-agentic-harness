package engine

import (
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

func writeVisualPNG(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func TestNativeVisualHelper(t *testing.T) {
	if os.Getenv("AIH_VISUAL_TEST_HELPER") != "1" {
		return
	}
	if os.Getenv("AIH_VISUAL_FAIL_HELPER") == "1" {
		os.Exit(3)
	}
	dir := os.Getenv("AIH_VISUAL_OUTPUT_DIR")
	if dir == "" {
		os.Exit(2)
	}
	_ = writeVisualPNG(filepath.Join(dir, "desktop.png"))
	_ = os.WriteFile(filepath.Join(dir, "network.txt"), []byte("GET /api-client.ts 404"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"summary":"blank page: frontend module 404","artifacts":["desktop.png","network.txt"]}`), 0600)
}

func TestNativeVisualCapturePinsHeadAndStoresOutsideSource(t *testing.T) {
	worktree, state := t.TempDir(), t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = worktree
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.txt")
	run("commit", "-m", "base")
	head := run("rev-parse", "HEAD")
	t.Setenv("AIH_VISUAL_TEST_HELPER", "1")
	c := &Controller{P: &Project{Dir: state}}
	effective := config.Effective{Hash: strings.Repeat("b", 64), Project: config.Project{VisualCapture: &config.VisualCapture{Command: []string{os.Args[0], "-test.run=^TestNativeVisualHelper$"}, Timeout: 10}}}
	task := &model.Task{ID: "task-visual", HeadSHA: head}
	if _, err := c.captureVisual(context.Background(), effective, &model.Task{ID: "../outside", HeadSHA: head}, worktree); err == nil {
		t.Fatal("unsafe task ID escaped evidence root")
	}
	t.Setenv("AIH_VISUAL_FAIL_HELPER", "1")
	if _, err := c.captureVisual(context.Background(), effective, task, worktree); err == nil {
		t.Fatal("failed capture accepted")
	}
	t.Setenv("AIH_VISUAL_FAIL_HELPER", "0")
	visual, err := c.captureVisual(context.Background(), effective, task, worktree)
	if err != nil || visual.Head != head || len(visual.Artifacts) != 2 {
		t.Fatalf("capture failed: %#v %v", visual, err)
	}
	if !strings.HasPrefix(filepath.Join(state, filepath.FromSlash(visual.Manifest)), state) {
		t.Fatal("manifest escaped AIH state")
	}
	if again, err := c.captureVisual(context.Background(), effective, task, worktree); err != nil || again.Manifest != visual.Manifest {
		t.Fatalf("exact-head cache missed: %#v %v", again, err)
	}
	if got := run("status", "--porcelain"); got != "" {
		t.Fatalf("capture dirtied source: %s", got)
	}
	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "source.txt")
	run("commit", "-m", "changed")
	if _, err := c.captureVisual(context.Background(), effective, task, worktree); err == nil {
		t.Fatal("stale task head reused visual evidence")
	}
}

func TestVisualEvidenceRequestRoutesBrowserSandboxFailure(t *testing.T) {
	request := provider.Result{Status: "blocked", Question: "Playwright launch returned EPERM. Can the supervisor provide screenshots and browser capture for this exact head?"}
	if !visualEvidenceRequest(request) || !supervisorEvidenceRequest(request) {
		t.Fatal("browser sandbox denial was not routed to supervisor evidence")
	}
	if visualEvidenceRequest(provider.Result{Status: "blocked", Question: "Choose whether to accept a product behavior change"}) {
		t.Fatal("product decision was treated as visual capture")
	}
}

func TestVisualManifestBoundsAndSecretRejection(t *testing.T) {
	dir := t.TempDir()
	head, cfg := strings.Repeat("a", 40), strings.Repeat("b", 64)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeVisualPNG(filepath.Join(dir, "frame.png")); err != nil {
		t.Fatal(err)
	}
	write("network.txt", "GET /api-client.ts 404")
	write("manifest.json", `{"summary":"blank page; frontend module returned 404","artifacts":["frame.png","network.txt"]}`)
	visual, err := loadVisualEvidence(dir, "task-1", head, cfg)
	if err != nil || len(visual.Artifacts) != 2 || visual.Head != head || !strings.HasPrefix(visual.Manifest, "visual-evidence/task-1/") {
		t.Fatalf("valid visual capture rejected: %#v %v", visual, err)
	}
	write("manifest.json", `{"summary":"bad","artifacts":["../secret.png"]}`)
	if _, err := loadVisualEvidence(dir, "task-1", head, cfg); err == nil {
		t.Fatal("traversal artifact accepted")
	}
	write("manifest.json", `{"summary":"bad","artifacts":["frame.png","network.txt"]}`)
	write("network.txt", "Authorization token="+strings.Repeat("A", 30))
	if _, err := loadVisualEvidence(dir, "task-1", head, cfg); err == nil {
		t.Fatal("secret-like diagnostic accepted")
	}
	write("network.txt", "GET /api 200")
	write("frame.png", "corrupt image")
	if _, err := loadVisualEvidence(dir, "task-1", head, cfg); err == nil {
		t.Fatal("corrupt screenshot accepted")
	}
}
