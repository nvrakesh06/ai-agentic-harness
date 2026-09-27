package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

const (
	maxNativeArtifactManifestBytes = 128 << 10
)

var (
	nativeArtifactReceiptReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{15,95}\.[a-f0-9]{64}$`)
	nativeArtifactHash             = regexp.MustCompile(`^[a-f0-9]{64}$`)
	nativeArtifactRevision         = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
)

// ErrNativeArtifactUnavailable means the portable validation record is valid
// but no matching local receipt can be read on this controller. It deliberately
// does not imply a visual approval or trigger a source repair route.
var ErrNativeArtifactUnavailable = errors.New("native artifact receipt unavailable")

// NativeArtifactInventory is an in-memory, controller-local inventory. Paths
// name validated local files and must never be persisted in Evidence or used as
// VisualEvidence.
type NativeArtifactInventory struct {
	Receipt string
	Images  []NativeArtifactInventoryImage
}

type NativeArtifactInventoryImage struct {
	Path          string
	SHA256        string
	Bytes         int64
	Width, Height int
}

// ResolveNativeArtifactInventory validates one compact receipt reference from
// current Evidence.Checks against its exact validation plan and local seal.
// The caller provides the project identity because it is controller-local, not
// task portable state. A missing attachment returns ErrNativeArtifactUnavailable.
func ResolveNativeArtifactInventory(project string, plan validationPlan, task *model.Task, evidence *model.Evidence, sealRoot, receipt string) (*NativeArtifactInventory, error) {
	if strings.TrimSpace(project) == "" || !nativeArtifactReceiptReference.MatchString(receipt) {
		return nil, fmt.Errorf("invalid native artifact receipt reference")
	}
	record, err := nativeArtifactEvidenceBinding(plan, task, evidence, receipt)
	if err != nil {
		return nil, err
	}
	dir, err := nativeArtifactReceiptDir(sealRoot, receipt)
	if err != nil {
		return nil, err
	}
	manifest, err := nativeArtifactReadManifest(dir, receipt)
	if err != nil {
		return nil, err
	}
	if err = nativeArtifactManifestBinding(manifest, project, plan, task, evidence, receipt); err != nil {
		return nil, err
	}
	if !nativeArtifactRecordMatchesCheck(record, plan.Checks[manifest.Check]) {
		return nil, errors.New("native artifact receipt is attached to a different check record")
	}
	images, err := nativeArtifactResolveImages(dir, manifest.Images)
	if err != nil {
		return nil, err
	}
	return &NativeArtifactInventory{Receipt: receipt, Images: images}, nil
}

func nativeArtifactEvidenceBinding(plan validationPlan, task *model.Task, evidence *model.Evidence, receipt string) (string, error) {
	if task == nil || task.ID == "" || evidence == nil || plan.ExpectedHead == "" || plan.ExpectedConfig == "" || plan.Input == "" || plan.Toolchain == "" || evidence.Head != plan.ExpectedHead || evidence.Config != plan.ExpectedConfig || evidence.Rules == "" || evidence.ValidationInput != plan.Input || evidence.Toolchain != plan.Toolchain {
		return "", errors.New("native artifact receipt does not match current validation evidence")
	}
	for _, record := range evidence.Checks {
		if strings.HasSuffix(record, " artifact="+receipt) {
			if !model.ValidPassedNativeCheckRecord(record) {
				return "", errors.New("native artifact reference is attached to an invalid check record")
			}
			return record, nil
		}
	}
	return "", fmt.Errorf("%w: receipt is absent from current checks", ErrNativeArtifactUnavailable)
}

func nativeArtifactRecordMatchesCheck(record string, check config.Check) bool {
	prefix := fmt.Sprintf("stage=native check=%q command=%q command_id=%s exit=0 ", check.Name, filepath.Base(check.Command[0]), nativeCheckCommandID(check))
	return strings.HasPrefix(record, prefix)
}

func nativeArtifactReceiptDir(sealRoot, receipt string) (string, error) {
	absRoot, err := filepath.Abs(sealRoot)
	if err != nil {
		return "", fmt.Errorf("resolve native artifact seal root: %w", err)
	}
	if err := nativeArtifactExistingSafeDir(absRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: seal root is absent", ErrNativeArtifactUnavailable)
		}
		return "", fmt.Errorf("unsafe native artifact seal root: %w", err)
	}
	id, _, _ := strings.Cut(receipt, ".")
	candidate := filepath.Join(absRoot, "receipt-"+id)
	if err := nativeArtifactExistingSafeDir(candidate); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: receipt directory is absent", ErrNativeArtifactUnavailable)
		}
		return "", fmt.Errorf("unsafe native artifact receipt directory: %w", err)
	}
	if !nativeArtifactWithin(absRoot, candidate) {
		return "", errors.New("native artifact receipt directory escapes seal root")
	}
	if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: receipt directory is absent", ErrNativeArtifactUnavailable)
	}
	return candidate, nil
}

func nativeArtifactReadManifest(dir, receipt string) (nativeArtifactManifest, error) {
	var manifest nativeArtifactManifest
	file := filepath.Join(dir, "manifest.json")
	if err := nativeArtifactExistingSafeDir(filepath.Dir(file)); err != nil {
		return manifest, err
	}
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return manifest, fmt.Errorf("%w: manifest is absent", ErrNativeArtifactUnavailable)
	}
	if err != nil || nativeArtifactUnsafeInfo(info) {
		return manifest, errors.New("native artifact manifest is not a safe regular file")
	}
	data, err := readNativeArtifactBytes(file, info, maxNativeArtifactManifestBytes)
	if err != nil {
		return manifest, fmt.Errorf("read native artifact manifest: %w", err)
	}
	if len(data) < 2 || data[len(data)-1] != '\n' {
		return manifest, errors.New("native artifact manifest has an invalid encoding")
	}
	payload := data[:len(data)-1]
	_, encodedHash, _ := strings.Cut(receipt, ".")
	hash := sha256.Sum256(payload)
	if hex.EncodeToString(hash[:]) != encodedHash {
		return manifest, errors.New("native artifact manifest hash differs from receipt")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&manifest); err != nil {
		return manifest, errors.New("native artifact manifest is not strict JSON")
	}
	var extra any
	// Decoder returns io.EOF for one JSON value. Anything else is a second
	// object or trailing non-whitespace bytes.
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return manifest, errors.New("native artifact manifest contains trailing data")
	}
	return manifest, nil
}

func nativeArtifactManifestBinding(manifest nativeArtifactManifest, project string, plan validationPlan, task *model.Task, evidence *model.Evidence, receipt string) error {
	id, _, _ := strings.Cut(receipt, ".")
	if manifest.Version != nativeArtifactManifestVersion || manifest.ID != id || manifest.Project != project || manifest.Task != task.ID || manifest.Check < 0 || manifest.Check >= len(plan.Checks) || manifest.Exit != 0 || manifest.StartedAt.IsZero() || manifest.FinishedAt.IsZero() || manifest.FinishedAt.Before(manifest.StartedAt) {
		return errors.New("native artifact manifest identity is invalid")
	}
	check := plan.Checks[manifest.Check]
	if !check.Artifacts || manifest.Digest != nativeArtifactCheckDigest(check) {
		return errors.New("native artifact manifest check is not currently opted in")
	}
	if !nativeArtifactRevision.MatchString(plan.ExpectedHead) || !nativeArtifactHash.MatchString(plan.ExpectedConfig) || !nativeArtifactHash.MatchString(plan.Input) || strings.TrimSpace(plan.Toolchain) == "" || !nativeArtifactHash.MatchString(evidence.Rules) {
		return errors.New("native artifact validation plan is malformed")
	}
	if manifest.Expected.Head != plan.ExpectedHead || manifest.Expected.Config != plan.ExpectedConfig || manifest.Expected.Rules != evidence.Rules || manifest.Expected.PlanInput != plan.Input || manifest.Expected.Toolchain != plan.Toolchain {
		return errors.New("native artifact manifest expected binding differs from current plan")
	}
	if manifest.Actual.HeadBefore != plan.ExpectedHead || manifest.Actual.HeadAfter != plan.ExpectedHead || !nativeArtifactRevision.MatchString(manifest.Actual.TreeBefore) || !nativeArtifactRevision.MatchString(manifest.Actual.TreeAfter) || manifest.Actual.TreeBefore != manifest.Actual.TreeAfter {
		return errors.New("native artifact manifest actual checkout binding is invalid")
	}
	if len(manifest.Images) == 0 || len(manifest.Images) > maxNativeArtifactImages {
		return errors.New("native artifact manifest image count is invalid")
	}
	return nil
}

func nativeArtifactResolveImages(dir string, images []nativeArtifactImage) ([]NativeArtifactInventoryImage, error) {
	resolved := make([]NativeArtifactInventoryImage, 0, len(images))
	total := int64(0)
	previous := ""
	for _, image := range images {
		if image.Path <= previous || !nativeArtifactSafeImagePath(image.Path) || !nativeArtifactHash.MatchString(image.SHA256) || image.Bytes < 1 || image.Bytes > maxNativeArtifactImageBytes || image.Width < 1 || image.Height < 1 || uint64(image.Width) > uint64(maxNativeArtifactPixels) || uint64(image.Height) > uint64(maxNativeArtifactPixels)/uint64(image.Width) {
			return nil, errors.New("native artifact manifest image is invalid")
		}
		previous = image.Path
		file := filepath.Join(dir, filepath.FromSlash(image.Path))
		if !nativeArtifactWithin(dir, file) {
			return nil, errors.New("native artifact image path escapes receipt")
		}
		if err := nativeArtifactExistingSafeDir(filepath.Dir(file)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: image directory is absent", ErrNativeArtifactUnavailable)
			}
			return nil, errors.New("native artifact image path escapes receipt")
		}
		info, err := os.Lstat(file)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: image is absent", ErrNativeArtifactUnavailable)
		}
		if err != nil || nativeArtifactUnsafeInfo(info) {
			return nil, errors.New("native artifact image is missing or unsafe")
		}
		data, width, height, err := readNativePNG(file, info)
		if err != nil || int64(len(data)) != image.Bytes || width != image.Width || height != image.Height {
			return nil, errors.New("native artifact image differs from manifest")
		}
		total += int64(len(data))
		if total > maxNativeArtifactTotalBytes {
			return nil, errors.New("native artifact images exceed total size limit")
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != image.SHA256 {
			return nil, errors.New("native artifact image hash differs from manifest")
		}
		resolved = append(resolved, NativeArtifactInventoryImage{Path: file, SHA256: image.SHA256, Bytes: image.Bytes, Width: width, Height: height})
	}
	return resolved, nil
}

func nativeArtifactSafeImagePath(value string) bool {
	return strings.HasSuffix(strings.ToLower(value), ".png") && len(value) <= maxNativeArtifactPathBytes && !strings.Contains(value, `\`) && !strings.HasPrefix(value, "/") && pathpkg.Clean(value) == value && !strings.Contains(value, "..") && strings.Count(value, "/") < maxNativeArtifactDepth && validNativeFailurePath(value)
}
