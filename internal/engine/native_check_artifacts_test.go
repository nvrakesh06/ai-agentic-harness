package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
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

func TestValidateNativePNGRejectsOverflowingDimensionsBeforeDecode(t *testing.T) {
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header, 0xffffffff)
	binary.BigEndian.PutUint32(header[4:], 0xffffffff)
	header[8], header[9], header[10] = 8, 6, 0
	data := append(append([]byte(nil), nativePNGSignature...), testPNGChunk("IHDR", header)...)
	data = append(data, testPNGChunk("IDAT", nil)...)
	data = append(data, testPNGChunk("IEND", nil)...)
	if _, _, err := validateNativePNG(data); err == nil {
		t.Fatal("overflowing valid-CRC dimensions accepted")
	}
}

func TestNativeArtifactStagingIsUniqueOutsideSource(t *testing.T) {
	space := t.TempDir()
	source, state := filepath.Join(space, "source"), filepath.Join(space, "state")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	bind := &nativeArtifactContext{StateRoot: state, SourceRoot: source}
	check := config.Check{Artifacts: true}
	first, firstCleanup, err := nativeArtifactStaging(check, bind)
	if err != nil {
		t.Fatal(err)
	}
	defer firstCleanup()
	second, secondCleanup, err := nativeArtifactStaging(check, bind)
	if err != nil {
		t.Fatal(err)
	}
	defer secondCleanup()
	if first == second || nativeArtifactWithin(source, first) || nativeArtifactWithin(source, second) {
		t.Fatalf("unsafe or reused staging paths: %q %q", first, second)
	}
}

func TestNativeArtifactFinalIdentityRejectsLaterCheckoutChanges(t *testing.T) {
	head, tree := strings.Repeat("a", 40), strings.Repeat("b", 40)
	bind := &nativeArtifactContext{ExpectedHead: head}
	pending := []*nativeArtifactPending{{afterHead: head, afterTree: tree}}
	if err := nativeArtifactFinalIdentityMatches(bind, head, tree, pending); err != nil {
		t.Fatal(err)
	}
	if err := nativeArtifactFinalIdentityMatches(bind, head, strings.Repeat("c", 40), pending); err == nil {
		t.Fatal("later checkout tree change accepted")
	}
	if err := nativeArtifactFinalIdentityMatches(bind, strings.Repeat("d", 40), tree, pending); err == nil {
		t.Fatal("later checkout HEAD change accepted")
	}
}

func TestImportNativeArtifactImagesRejectsHardLinks(t *testing.T) {
	root := t.TempDir()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(root, "one.png"), filepath.Join(root, "two.png")
	if err := os.WriteFile(first, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := importNativeArtifactImages(root); err == nil {
		t.Fatal("hard-linked image accepted")
	}
}

func TestArtifactRootAndSealRootRejectLinks(t *testing.T) {
	space := t.TempDir()
	target, linked := filepath.Join(space, "target"), filepath.Join(space, "linked")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linked); err != nil {
		t.Skipf("links unavailable: %v", err)
	}
	if _, err := importNativeArtifactImages(linked); err == nil {
		t.Fatal("linked staging root accepted")
	}
	source := filepath.Join(space, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	bind := &nativeArtifactContext{StateRoot: filepath.Join(space, "state"), SourceRoot: source, SealRoot: linked}
	if err := nativeArtifactPrepareSealRoot(bind); err == nil {
		t.Fatal("linked seal root accepted")
	}
}

func TestSealNativeArtifactsWritesReceiptOutsideStaging(t *testing.T) {
	space := t.TempDir()
	staging := filepath.Join(space, "staging")
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
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
	source := filepath.Join(space, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(space, "state")
	bind := &nativeArtifactContext{ExpectedHead: strings.Repeat("a", 40), Config: strings.Repeat("c", 64), Rules: strings.Repeat("d", 64), PlanInput: strings.Repeat("e", 64), Toolchain: "test=hash", Project: "project", Task: "task", StateRoot: state, SourceRoot: source, SealRoot: filepath.Join(state, "seal")}
	receipt, err := sealNativeArtifacts(pending, bind)
	if err != nil || !strings.Contains(receipt, ".") {
		t.Fatalf("seal receipt = %q, %v", receipt, err)
	}
	if parts := strings.Split(receipt, "."); len(parts) != 2 || len(parts[1]) != 64 {
		t.Fatalf("receipt does not contain full manifest hash: %q", receipt)
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

func TestSealNativeArtifactsKeepsInvocationBytesAndHeadsIndependent(t *testing.T) {
	space := t.TempDir()
	source, state := filepath.Join(space, "source"), filepath.Join(space, "state")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	bind := &nativeArtifactContext{ExpectedHead: strings.Repeat("a", 40), Config: strings.Repeat("c", 64), Rules: strings.Repeat("d", 64), PlanInput: strings.Repeat("e", 64), Toolchain: "test=hash", Project: "project", Task: "task", StateRoot: state, SourceRoot: source, SealRoot: filepath.Join(state, "seal")}
	first := sealFixturePending(t, space, "first", color.RGBA{R: 255, A: 255}, strings.Repeat("a", 40))
	second := sealFixturePending(t, space, "second", color.RGBA{B: 255, A: 255}, strings.Repeat("b", 40))
	firstReceipt, err := sealNativeArtifacts(first, bind)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, err := sealNativeArtifacts(second, bind)
	if err != nil {
		t.Fatal(err)
	}
	if firstReceipt == secondReceipt {
		t.Fatalf("distinct invocations shared receipt %q", firstReceipt)
	}
	entries, err := os.ReadDir(bind.SealRoot)
	if err != nil || len(entries) != 2 {
		t.Fatalf("receipt inventory = %#v, %v", entries, err)
	}
	manifests := map[string]nativeArtifactManifest{}
	for _, entry := range entries {
		data, readErr := os.ReadFile(filepath.Join(bind.SealRoot, entry.Name(), "manifest.json"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var manifest nativeArtifactManifest
		if unmarshalErr := json.Unmarshal(data, &manifest); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		manifests[manifest.Actual.HeadBefore] = manifest
	}
	left, right := manifests[strings.Repeat("a", 40)], manifests[strings.Repeat("b", 40)]
	if len(left.Images) != 1 || len(right.Images) != 1 || left.Images[0].SHA256 == right.Images[0].SHA256 || left.Actual.HeadAfter != left.Actual.HeadBefore || right.Actual.HeadAfter != right.Actual.HeadBefore {
		t.Fatalf("invocation provenance or image bytes were conflated: left=%#v right=%#v", left, right)
	}
}

func TestImportNativeArtifactImagesRejectsImageCountLimit(t *testing.T) {
	root := t.TempDir()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxNativeArtifactImages; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("frame-%d.png", i)), encoded.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := importNativeArtifactImages(root); err == nil {
		t.Fatal("image count limit accepted")
	}
}

func testPNGChunk(kind string, body []byte) []byte {
	chunk := make([]byte, 12+len(body))
	binary.BigEndian.PutUint32(chunk, uint32(len(body)))
	copy(chunk[4:], kind)
	copy(chunk[8:], body)
	binary.BigEndian.PutUint32(chunk[8+len(body):], crc32.ChecksumIEEE(chunk[4:8+len(body)]))
	return chunk
}

func sealFixturePending(t *testing.T, parent, name string, pixel color.RGBA, head string) *nativeArtifactPending {
	t.Helper()
	staging := filepath.Join(parent, name)
	if err := os.Mkdir(staging, 0700); err != nil {
		t.Fatal(err)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 1, 1))
	imageData.SetRGBA(0, 0, pixel)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageData); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "frame.png"), encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return &nativeArtifactPending{check: config.Check{Name: "browser", Command: []string{"test"}, Artifacts: true}, staging: staging, started: time.Now().UTC(), finished: time.Now().UTC(), beforeHead: head, beforeTree: strings.Repeat("f", 40), afterHead: head, afterTree: strings.Repeat("f", 40)}
}
