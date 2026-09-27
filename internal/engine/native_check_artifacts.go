package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

const (
	nativeCheckArtifactsEnv       = "AIH_CHECK_ARTIFACTS"
	maxNativeArtifactImages       = 64
	maxNativeArtifactImageBytes   = 8 << 20
	maxNativeArtifactTotalBytes   = 64 << 20
	maxNativeArtifactEntries      = 512
	maxNativeArtifactDepth        = 8
	maxNativeArtifactPathBytes    = 240
	maxNativeArtifactPixels       = 16 << 20
	maxNativeArtifactPNGChunks    = 4096
	nativeArtifactManifestVersion = 1
)

var nativePNGSignature = []byte{137, 80, 78, 71, 13, 10, 26, 10}

// nativeArtifactContext exists only while an already-bound validation plan is
// executing. It deliberately cannot be reconstructed from portable evidence.
type nativeArtifactContext struct {
	ExpectedHead, Config, Rules string
	PlanInput, Toolchain        string
	Project, Task, SealRoot     string
}

type nativeArtifactPending struct {
	check      config.Check
	index      int
	evidence   int
	staging    string
	cleanup    func()
	started    time.Time
	finished   time.Time
	beforeHead string
	beforeTree string
	afterHead  string
	afterTree  string
}

type nativeArtifactImage struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	data   []byte
}

type nativeArtifactManifest struct {
	Version  int    `json:"version"`
	ID       string `json:"id"`
	Project  string `json:"project"`
	Task     string `json:"task"`
	Check    int    `json:"check_index"`
	Digest   string `json:"check_digest"`
	Expected struct {
		Head, Config, Rules, PlanInput, Toolchain string
	} `json:"expected"`
	Actual struct {
		HeadBefore, TreeBefore, HeadAfter, TreeAfter string
	} `json:"actual"`
	Exit       int                   `json:"exit"`
	StartedAt  time.Time             `json:"started_at"`
	FinishedAt time.Time             `json:"finished_at"`
	Images     []nativeArtifactImage `json:"images"`
}

func nativeArtifactStaging(check config.Check) (string, func(), error) {
	if !check.Artifacts {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp("", "aih-check-artifacts-")
	if err != nil {
		return "", nil, err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func nativeCheckEnvironment(base []string, report, artifacts string) []string {
	env := make([]string, 0, len(base)+2)
	for _, item := range base {
		key, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(key, nativeFailureReportEnv) || strings.EqualFold(key, nativeCheckArtifactsEnv) {
			continue
		}
		env = append(env, item)
	}
	if report != "" {
		env = append(env, nativeFailureReportEnv+"="+report)
	}
	if artifacts != "" {
		env = append(env, nativeCheckArtifactsEnv+"="+artifacts)
	}
	return env
}

func checkoutIdentity(ctx context.Context, dir string) (string, string, error) {
	g := gitx.Git{Dir: dir}
	head, err := g.SHA(ctx, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("read verification checkout HEAD: %w", err)
	}
	tree, err := g.Run(ctx, "", "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("read verification checkout tree: %w", err)
	}
	return head, tree, nil
}

func beginNativeArtifacts(ctx context.Context, check config.Check, index int, dir string, bind *nativeArtifactContext) (*nativeArtifactPending, error) {
	if !check.Artifacts {
		return nil, nil
	}
	if bind == nil || bind.ExpectedHead == "" || bind.Config == "" || bind.Rules == "" || bind.PlanInput == "" || bind.Toolchain == "" || bind.Project == "" || bind.Task == "" || bind.SealRoot == "" {
		return nil, errors.New("native artifact check requires an exact validation-plan context")
	}
	head, tree, err := checkoutIdentity(ctx, dir)
	if err != nil {
		return nil, err
	}
	if head != bind.ExpectedHead {
		return nil, errors.New("native artifact checkout HEAD differs from validation plan")
	}
	staging, cleanup, err := nativeArtifactStaging(check)
	if err != nil {
		return nil, fmt.Errorf("allocate native artifact staging: %w", err)
	}
	return &nativeArtifactPending{check: check, index: index, staging: staging, cleanup: cleanup, started: time.Now().UTC(), beforeHead: head, beforeTree: tree}, nil
}

func finishNativeArtifacts(ctx context.Context, pending *nativeArtifactPending, dir string, bind *nativeArtifactContext) error {
	if pending == nil {
		return nil
	}
	pending.finished = time.Now().UTC()
	head, tree, err := checkoutIdentity(ctx, dir)
	if err != nil {
		return err
	}
	pending.afterHead, pending.afterTree = head, tree
	if head != bind.ExpectedHead || head != pending.beforeHead || tree != pending.beforeTree {
		return errors.New("native artifact checkout identity changed during check")
	}
	return nil
}

func nativeArtifactCheckDigest(check config.Check) string {
	b, _ := json.Marshal(check)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sealNativeArtifacts(pending *nativeArtifactPending, bind *nativeArtifactContext) (string, error) {
	if pending == nil || pending.staging == "" {
		return "", nil
	}
	images, err := importNativeArtifactImages(pending.staging)
	if err != nil {
		return "", err
	}
	if len(images) == 0 {
		return "", nil
	}
	if err = os.MkdirAll(bind.SealRoot, 0700); err != nil {
		return "", fmt.Errorf("create native artifact seal root: %w", err)
	}
	if err = os.Chmod(bind.SealRoot, 0700); err != nil {
		return "", err
	}
	id := model.ID()
	dir, err := os.MkdirTemp(bind.SealRoot, "receipt-"+id+"-")
	if err != nil {
		return "", fmt.Errorf("allocate native artifact receipt: %w", err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(dir)
		}
	}()
	manifest := nativeArtifactManifest{Version: nativeArtifactManifestVersion, ID: id, Project: bind.Project, Task: bind.Task, Check: pending.index, Digest: nativeArtifactCheckDigest(pending.check), Exit: 0, StartedAt: pending.started, FinishedAt: pending.finished, Images: images}
	manifest.Expected.Head, manifest.Expected.Config, manifest.Expected.Rules, manifest.Expected.PlanInput, manifest.Expected.Toolchain = bind.ExpectedHead, bind.Config, bind.Rules, bind.PlanInput, bind.Toolchain
	manifest.Actual.HeadBefore, manifest.Actual.TreeBefore, manifest.Actual.HeadAfter, manifest.Actual.TreeAfter = pending.beforeHead, pending.beforeTree, pending.afterHead, pending.afterTree
	for _, image := range images {
		if copyErr := writeNativeArtifact(filepath.Join(dir, filepath.FromSlash(image.Path)), image.data); copyErr != nil {
			return "", copyErr
		}
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(payload)
	if err = writeNativeArtifact(filepath.Join(dir, "manifest.json"), append(payload, '\n')); err != nil {
		return "", err
	}
	if err = sealNativeArtifactTree(dir); err != nil {
		return "", err
	}
	remove = false
	return id + "." + hex.EncodeToString(hash[:])[:12], nil
}

func writeNativeArtifact(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	return errors.Join(err, closeErr)
}

func sealNativeArtifactTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0500)
		}
		return os.Chmod(path, 0400)
	})
}

func importNativeArtifactImages(root string) ([]nativeArtifactImage, error) {
	var images []nativeArtifactImage
	entries, total := 0, int64(0)
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxNativeArtifactEntries {
			return errors.New("native artifact directory has too many entries")
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if len(rel) > maxNativeArtifactPathBytes || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") || strings.Count(rel, "/") >= maxNativeArtifactDepth {
			return errors.New("native artifact path is unsafe or exceeds bounds")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return errors.New("native artifact contains a link or reparse point")
			}
			return nil
		}
		if nativeArtifactUnsafeInfo(info) {
			return errors.New("native artifact contains a link, reparse point, or nonregular file")
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".png") {
			if len(images) >= maxNativeArtifactImages {
				return errors.New("native artifact image count exceeds limit")
			}
			data, width, height, err := readNativePNG(file, info)
			if err != nil {
				return fmt.Errorf("invalid native PNG %s: %w", rel, err)
			}
			total += int64(len(data))
			if total > maxNativeArtifactTotalBytes {
				return errors.New("native artifact images exceed total size limit")
			}
			sum := sha256.Sum256(data)
			images = append(images, nativeArtifactImage{Path: rel, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Width: width, Height: height, data: data})
			return nil
		}
		if ignoredNativeArtifactMetadata(entry.Name()) {
			return nil
		}
		return errors.New("native artifact output is not a PNG or allowed metadata")
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Path < images[j].Path })
	return images, nil
}

func ignoredNativeArtifactMetadata(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".json", ".log":
		return true
	default:
		return false
	}
}

func readNativePNG(path string, before os.FileInfo) ([]byte, int, int, error) {
	if before.Size() < int64(len(nativePNGSignature)) || before.Size() > maxNativeArtifactImageBytes {
		return nil, 0, 0, errors.New("file size exceeds PNG bounds")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || nativeArtifactUnsafeInfo(opened) || !nativeArtifactOpenedSafe(f) || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		return nil, 0, 0, errors.New("file identity changed before inspection")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxNativeArtifactImageBytes+1))
	if err != nil {
		return nil, 0, 0, err
	}
	after, err := f.Stat()
	if err != nil || nativeArtifactUnsafeInfo(after) || !nativeArtifactOpenedSafe(f) || !os.SameFile(opened, after) || after.Size() != int64(len(data)) {
		return nil, 0, 0, errors.New("file identity changed during inspection")
	}
	width, height, err := validateNativePNG(data)
	return data, width, height, err
}

func validateNativePNG(data []byte) (int, int, error) {
	if len(data) < 33 || !bytes.Equal(data[:8], nativePNGSignature) {
		return 0, 0, errors.New("missing PNG signature")
	}
	position, chunks, width, height, haveIHDR, haveIDAT := 8, 0, 0, 0, false, false
	for position < len(data) {
		if len(data)-position < 12 {
			return 0, 0, errors.New("truncated PNG chunk")
		}
		length := int(binary.BigEndian.Uint32(data[position:]))
		position += 4
		kind := data[position : position+4]
		position += 4
		if length < 0 || length > len(data)-position-4 || chunks >= maxNativeArtifactPNGChunks {
			return 0, 0, errors.New("invalid PNG chunk length")
		}
		body := data[position : position+length]
		position += length
		actualCRC := binary.BigEndian.Uint32(data[position:])
		position += 4
		if crc32.ChecksumIEEE(append(append([]byte(nil), kind...), body...)) != actualCRC {
			return 0, 0, errors.New("PNG chunk CRC mismatch")
		}
		chunks++
		name := string(kind)
		switch name {
		case "IHDR":
			if haveIHDR || chunks != 1 || length != 13 {
				return 0, 0, errors.New("invalid PNG header")
			}
			width, height = int(binary.BigEndian.Uint32(body)), int(binary.BigEndian.Uint32(body[4:]))
			if width < 1 || height < 1 || int64(width)*int64(height) > maxNativeArtifactPixels {
				return 0, 0, errors.New("PNG dimensions exceed bounds")
			}
			haveIHDR = true
		case "IDAT":
			if !haveIHDR {
				return 0, 0, errors.New("PNG data precedes header")
			}
			haveIDAT = true
		case "IEND":
			if !haveIHDR || !haveIDAT || length != 0 || position != len(data) {
				return 0, 0, errors.New("invalid PNG end")
			}
			image, err := png.Decode(bytes.NewReader(data))
			if err != nil || image.Bounds().Dx() != width || image.Bounds().Dy() != height {
				return 0, 0, errors.New("PNG pixels are malformed")
			}
			return width, height, nil
		}
	}
	return 0, 0, errors.New("PNG has no end chunk")
}
