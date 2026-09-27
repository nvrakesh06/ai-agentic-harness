package engine

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
)

func TestNativeCheckEnvironmentReplacesInheritedArtifactPath(t *testing.T) {
	env := nativeCheckEnvironment([]string{"AIH_CHECK_ARTIFACTS=inherited", "aih_failure_report=old", "SAFE=value"}, "report", "fresh")
	joined := strings.Join(env, "\n")
	if strings.Count(strings.ToUpper(joined), "AIH_CHECK_ARTIFACTS=") != 1 || !strings.Contains(joined, "AIH_CHECK_ARTIFACTS=fresh") || strings.Count(strings.ToUpper(joined), "AIH_FAILURE_REPORT=") != 1 || !strings.Contains(joined, "SAFE=value") {
		t.Fatalf("unexpected native check environment: %q", env)
	}
	disabled := strings.Join(nativeCheckEnvironment([]string{"AIH_CHECK_ARTIFACTS=inherited"}, "", ""), "\n")
	if strings.Contains(strings.ToUpper(disabled), "AIH_CHECK_ARTIFACTS=") {
		t.Fatalf("disabled check retained inherited artifact path: %q", disabled)
	}
}

func TestImportNativeArtifactImagesAcceptsOnlyBoundedPNGs(t *testing.T) {
	root := t.TempDir()
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frame.png"), pngBytes.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("diagnostic"), 0600); err != nil {
		t.Fatal(err)
	}
	images, err := importNativeArtifactImages(root)
	if err != nil || len(images) != 1 || images[0].Width != 2 || images[0].Height != 3 || images[0].SHA256 == "" {
		t.Fatalf("valid PNG import = %#v, %v", images, err)
	}
	if err := os.WriteFile(filepath.Join(root, "unsupported.jpg"), []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := importNativeArtifactImages(root); err == nil {
		t.Fatal("unsupported artifact output accepted")
	}
}

func TestSealNativeArtifactsWritesReceiptOutsideStaging(t *testing.T) {
	staging := t.TempDir()
	var pngBytes bytes.Buffer
	imageData := image.NewRGBA(image.Rect(0, 0, 1, 1))
	imageData.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&pngBytes, imageData); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "frame.png"), pngBytes.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	pending := &nativeArtifactPending{check: config.Check{Name: "browser", Command: []string{"test"}, Artifacts: true}, index: 2, staging: staging, started: time.Now().UTC(), finished: time.Now().UTC(), beforeHead: strings.Repeat("a", 40), beforeTree: strings.Repeat("b", 40), afterHead: strings.Repeat("a", 40), afterTree: strings.Repeat("b", 40)}
	bind := &nativeArtifactContext{ExpectedHead: strings.Repeat("a", 40), Config: strings.Repeat("c", 64), Rules: strings.Repeat("d", 64), PlanInput: strings.Repeat("e", 64), Toolchain: "test=hash", Project: "project", Task: "task", SealRoot: filepath.Join(t.TempDir(), "seal")}
	receipt, err := sealNativeArtifacts(pending, bind)
	if err != nil || !strings.Contains(receipt, ".") {
		t.Fatalf("seal receipt = %q, %v", receipt, err)
	}
	entries, err := os.ReadDir(bind.SealRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("seal entries = %#v, %v", entries, err)
	}
	if _, err = os.Stat(filepath.Join(bind.SealRoot, entries[0].Name(), "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(bind.SealRoot, entries[0].Name(), "frame.png")); err != nil {
		t.Fatal(err)
	}
}
