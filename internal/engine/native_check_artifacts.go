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
	Project, Task               string
	// StateRoot is controller-owned and must resolve outside SourceRoot. Both
	// staging and receipts stay beneath it rather than trusting TMP/TEMP.
	StateRoot, SourceRoot, SealRoot string
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

func nativeArtifactStaging(check config.Check, bind *nativeArtifactContext) (string, func(), error) {
	if !check.Artifacts {
		return "", func() {}, nil
	}
	if err := nativeArtifactPrepareStateRoot(bind); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(bind.StateRoot, "native-check-artifacts-")
	if err != nil {
		return "", nil, err
	}
	if err = nativeArtifactPathSafe(dir, true); err == nil {
		err = os.Chmod(dir, 0700)
	}
	if err != nil {
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

func nativeArtifactPrepareStateRoot(bind *nativeArtifactContext) error {
	if bind == nil || bind.StateRoot == "" || bind.SourceRoot == "" {
		return errors.New("native artifact state root is not bound")
	}
	if err := nativeArtifactEnsureSafeDir(bind.StateRoot); err != nil {
		return fmt.Errorf("prepare native artifact state root: %w", err)
	}
	if err := nativeArtifactOutsideSource(bind.SourceRoot, bind.StateRoot); err != nil {
		return err
	}
	return nil
}

func nativeArtifactPrepareSealRoot(bind *nativeArtifactContext) error {
	if err := nativeArtifactPrepareStateRoot(bind); err != nil {
		return err
	}
	if err := nativeArtifactEnsureSafeDir(bind.SealRoot); err != nil {
		return fmt.Errorf("prepare native artifact seal root: %w", err)
	}
	if err := nativeArtifactOutsideSource(bind.SourceRoot, bind.SealRoot); err != nil {
		return err
	}
	if !nativeArtifactWithin(bind.StateRoot, bind.SealRoot) {
		return errors.New("native artifact seal root is outside controller state")
	}
	return os.Chmod(bind.SealRoot, 0700)
}

func nativeArtifactEnsureSafeDir(path string) error {
	return nativeArtifactWalkSafeDirs(path, true)
}

// nativeArtifactExistingSafeDir performs the same ancestor validation as the
// writer path without creating anything. Local receipt resolution must never
// turn an attach-loss or stale reference into a filesystem mutation.
func nativeArtifactExistingSafeDir(path string) error {
	return nativeArtifactWalkSafeDirs(path, false)
}

func nativeArtifactWalkSafeDirs(path string, create bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root, rest := nativeArtifactPathRoot(abs)
	if root == "" {
		return fmt.Errorf("native artifact directory root is unavailable: %s", abs)
	}
	if err = nativeArtifactPathSafe(root, true); err != nil {
		return fmt.Errorf("unsafe native artifact directory ancestor %s: %w", root, err)
	}
	current := root
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if !info.IsDir() {
				return errors.New("native artifact directory is not a directory")
			}
		} else if errors.Is(statErr, os.ErrNotExist) {
			if !create {
				return statErr
			}
			if mkdirErr := os.Mkdir(current, 0700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
		} else {
			return statErr
		}
		if safeErr := nativeArtifactPathSafe(current, true); safeErr != nil {
			return fmt.Errorf("unsafe native artifact directory ancestor %s: %w", current, safeErr)
		}
	}
	return nil
}

// nativeArtifactPathRoot separates a volume-aware absolute root from the
// remaining components. On Windows this validates C:\\ (or a UNC share root)
// as an actual directory before inspecting every child; on Unix it starts at
// / and performs the same walk.
func nativeArtifactPathRoot(abs string) (string, string) {
	volume := filepath.VolumeName(abs)
	root := string(filepath.Separator)
	if volume != "" {
		root = volume + string(filepath.Separator)
	}
	if !strings.HasPrefix(abs, root) {
		return "", ""
	}
	return root, strings.TrimPrefix(abs, root)
}

func nativeArtifactOutsideSource(source, candidate string) error {
	sourceResolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return fmt.Errorf("resolve source root for native artifacts: %w", err)
	}
	candidateResolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve native artifact root: %w", err)
	}
	if nativeArtifactWithin(sourceResolved, candidateResolved) {
		return errors.New("native artifact directory must be outside the source checkout")
	}
	return nil
}

func nativeArtifactWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
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
	if bind == nil || bind.ExpectedHead == "" || bind.Config == "" || bind.Rules == "" || bind.PlanInput == "" || bind.Toolchain == "" || bind.Project == "" || bind.Task == "" || bind.StateRoot == "" || bind.SourceRoot == "" || bind.SealRoot == "" {
		return nil, errors.New("native artifact check requires an exact validation-plan context")
	}
	head, tree, err := checkoutIdentity(ctx, dir)
	if err != nil {
		return nil, err
	}
	if head != bind.ExpectedHead {
		return nil, errors.New("native artifact checkout HEAD differs from validation plan")
	}
	staging, cleanup, err := nativeArtifactStaging(check, bind)
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

// finalNativeArtifactIdentity runs after every check and the ordinary source
// cleanliness fence. A later non-artifact check therefore cannot invalidate a
// receipt that an earlier opted-in check happened to finish successfully.
func finalNativeArtifactIdentity(ctx context.Context, dir string, bind *nativeArtifactContext, pending []*nativeArtifactPending) error {
	head, tree, err := checkoutIdentity(ctx, dir)
	if err != nil {
		return err
	}
	return nativeArtifactFinalIdentityMatches(bind, head, tree, pending)
}

func nativeArtifactFinalIdentityMatches(bind *nativeArtifactContext, head, tree string, pending []*nativeArtifactPending) error {
	if bind == nil || head != bind.ExpectedHead {
		return errors.New("native artifact checkout HEAD differs after verification")
	}
	for _, item := range pending {
		if item == nil || item.afterHead != head || item.afterTree != tree {
			return errors.New("native artifact checkout identity changed after check completion")
		}
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
	if err = nativeArtifactPrepareSealRoot(bind); err != nil {
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
	return id + "." + hex.EncodeToString(hash[:]), nil
}

func writeNativeArtifact(path string, data []byte) error {
	if err := nativeArtifactEnsureSafeDir(filepath.Dir(path)); err != nil {
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
	if err := nativeArtifactPathSafe(root, true); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = nativeArtifactPathSafe(path, entry.IsDir()); err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0500)
		}
		return os.Chmod(path, 0400)
	})
}

func importNativeArtifactImages(root string) ([]nativeArtifactImage, error) {
	if err := nativeArtifactPathSafe(root, true); err != nil {
		return nil, err
	}
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
		if err := nativeArtifactPathSafe(file, entry.IsDir()); err != nil {
			return err
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
	data, err := readNativeArtifactBytes(path, before, maxNativeArtifactImageBytes)
	if err != nil {
		return nil, 0, 0, err
	}
	width, height, err := validateNativePNG(data)
	return data, width, height, err
}

func readNativeArtifactBytes(path string, before os.FileInfo, limit int64) ([]byte, error) {
	if before == nil || before.Size() < 1 || before.Size() > limit {
		return nil, errors.New("file size exceeds bounds")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || nativeArtifactUnsafeInfo(opened) || !nativeArtifactOpenedSafe(f) || !os.SameFile(before, opened) || opened.Size() != before.Size() {
		return nil, errors.New("file identity changed before inspection")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || nativeArtifactUnsafeInfo(after) || !nativeArtifactOpenedSafe(f) || !os.SameFile(opened, after) || after.Size() != int64(len(data)) {
		return nil, errors.New("file identity changed during inspection")
	}
	return data, nil
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
			w, h := uint64(binary.BigEndian.Uint32(body)), uint64(binary.BigEndian.Uint32(body[4:]))
			if w == 0 || h == 0 || w > uint64(maxNativeArtifactPixels) || h > uint64(maxNativeArtifactPixels)/w {
				return 0, 0, errors.New("PNG dimensions exceed bounds")
			}
			width, height = int(w), int(h)
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
