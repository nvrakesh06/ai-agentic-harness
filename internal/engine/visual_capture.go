package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

const visualFileLimit = 8 << 20

// The runner is deliberately AIH-owned. It creates a fresh browser context,
// pins its viewport/channel, and aborts every request whose origin differs
// from the configured loopback origin, including redirects, subresources, and
// WebSockets. Project configuration supplies only the loopback URL.
const visualRunner = `
import { createRequire } from 'node:module';
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
const require = createRequire(process.cwd() + '/package.json');
const { chromium } = require('playwright');
const output = process.env.AIH_VISUAL_OUTPUT_DIR;
const head = process.env.AIH_VISUAL_HEAD;
const target = process.env.AIH_VISUAL_URL;
const origin = new URL(target).origin;
const network = [];
const browser = await chromium.launch({ channel: 'chrome', headless: true });
const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
await context.route('**/*', async route => {
  const request = route.request();
  const url = new URL(request.url());
  if (url.origin !== origin) { network.push('BLOCKED ' + request.method() + ' ' + url.origin); return route.abort('blockedbyclient'); }
  return route.continue();
});
context.on('response', response => network.push(response.request().method() + ' ' + response.status() + ' ' + new URL(response.url()).pathname));
const page = await context.newPage();
await page.goto(target, { waitUntil: 'networkidle' });
await page.screenshot({ path: join(output, 'desktop.png') });
writeFileSync(join(output, 'network.txt'), network.join('\n'));
writeFileSync(join(output, 'manifest.json'), JSON.stringify({ head, summary: 'AIH-owned browser capture', artifacts: ['desktop.png', 'network.txt'] }));
await context.close();
await browser.close();
`

// visualCaptureUnavailableError denotes a supervisor capability problem. It
// must never enter the implementer FIX loop because no source change can add a
// missing capture command or repair its local launch environment.
type visualCaptureUnavailableError struct{ err error }

func (e *visualCaptureUnavailableError) Error() string {
	return "visual capture unavailable: " + e.err.Error()
}
func (e *visualCaptureUnavailableError) Unwrap() error { return e.err }

func visualCaptureRunError(command string, err error, output string) error {
	if errors.Is(err, exec.ErrNotFound) {
		return &visualCaptureUnavailableError{fmt.Errorf("%w: %s", err, filepath.Base(command))}
	}
	return &checkFailure{name: "visual capture", command: filepath.Base(command), err: err, output: short(safety.Redact(output), 2000)}
}

type visualManifest struct {
	Head      string   `json:"head"`
	Summary   string   `json:"summary"`
	Artifacts []string `json:"artifacts"`
}

var visualName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}\.(png|jpg|jpeg|txt|json)$`)
var visualTaskID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`)
var visualRevision = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var visualHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func visualArtifact(root, name string) (model.VisualArtifact, error) {
	if !visualName.MatchString(name) || strings.Contains(name, "..") {
		return model.VisualArtifact{}, errors.New("visual artifact name must be a flat safe filename")
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if err != nil {
		return model.VisualArtifact{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > visualFileLimit {
		return model.VisualArtifact{}, errors.New("visual artifact must be a regular file <= 8 MiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return model.VisualArtifact{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, io.LimitReader(f, visualFileLimit+1)); err != nil {
		return model.VisualArtifact{}, err
	}
	if strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") {
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return model.VisualArtifact{}, err
		}
		imageConfig, _, decodeErr := image.DecodeConfig(f)
		if decodeErr != nil || imageConfig.Width < 1 || imageConfig.Height < 1 || imageConfig.Width > 4096 || imageConfig.Height > 4096 {
			return model.VisualArtifact{}, errors.New("visual screenshot must be a valid image <= 4096x4096")
		}
	}
	if strings.HasSuffix(name, ".txt") || strings.HasSuffix(name, ".json") {
		body, err := os.ReadFile(path)
		if err != nil {
			return model.VisualArtifact{}, err
		}
		if err = safety.Check(string(body)); err != nil {
			return model.VisualArtifact{}, err
		}
	}
	return model.VisualArtifact{Path: name, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func loadVisualEvidence(dir, taskID, head, configHash string) (*model.VisualEvidence, error) {
	manifestPath := filepath.Join(dir, "manifest.json")
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<10 {
		return nil, errors.New("visual manifest must be a regular file <= 32 KiB")
	}
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	if err = safety.Check(string(body)); err != nil {
		return nil, err
	}
	var m visualManifest
	if err = json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m.Head != head {
		return nil, errors.New("visual manifest captured head differs from reviewed head")
	}
	if len(m.Summary) > 1000 || len(m.Artifacts) == 0 || len(m.Artifacts) > 8 {
		return nil, errors.New("visual manifest needs a bounded summary and 1..8 artifacts")
	}
	seen := map[string]bool{}
	artifacts := make([]model.VisualArtifact, 0, len(m.Artifacts))
	var total int64
	image := false
	for _, name := range m.Artifacts {
		if seen[name] {
			return nil, errors.New("duplicate visual artifact")
		}
		seen[name] = true
		artifact, err := visualArtifact(dir, name)
		if err != nil {
			return nil, err
		}
		info, _ := os.Stat(filepath.Join(dir, name))
		total += info.Size()
		if total > 16<<20 {
			return nil, errors.New("visual artifacts exceed 16 MiB")
		}
		if strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") {
			image = true
		}
		artifacts = append(artifacts, artifact)
	}
	if !image {
		return nil, errors.New("visual manifest contains no screenshot")
	}
	manifestHash := sha256.Sum256(body)
	return &model.VisualEvidence{Head: head, Config: configHash, Manifest: filepath.ToSlash(filepath.Join("visual-evidence", taskID, head+"-"+configHash[:16], "manifest.json")), ManifestSHA256: hex.EncodeToString(manifestHash[:]), Artifacts: artifacts, Summary: m.Summary}, nil
}

// The supervisor writes this seal after validation. On reuse, compare the
// manifest and every artifact hash with the original accepted capture.
func sealVisualEvidence(dir string, evidence *model.VisualEvidence) error {
	f, err := os.OpenFile(filepath.Join(dir, "capture-seal.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(evidence)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func loadSealedVisualEvidence(dir, taskID, head, configHash string) (*model.VisualEvidence, error) {
	info, err := os.Lstat(filepath.Join(dir, "capture-seal.json"))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<10 {
		return nil, errors.New("visual capture seal must be a regular file <= 32 KiB")
	}
	body, err := os.ReadFile(filepath.Join(dir, "capture-seal.json"))
	if err != nil {
		return nil, err
	}
	var sealed model.VisualEvidence
	if err = json.Unmarshal(body, &sealed); err != nil {
		return nil, err
	}
	current, err := loadVisualEvidence(dir, taskID, head, configHash)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(sealed, *current) {
		return nil, errors.New("cached visual capture differs from its accepted manifest or artifact hashes")
	}
	return current, nil
}

// quarantineVisualCapture preserves one rejected cache for local diagnosis,
// then permits one clean recapture at the same head/config. A second corrupt
// cache is terminal so cache damage cannot create an endless recapture loop.
func quarantineVisualCapture(output string) error {
	quarantine := output + ".corrupt"
	if _, err := os.Lstat(quarantine); err == nil {
		return errors.New("visual capture cache was already recaptured once")
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(output, quarantine)
}

// captureVisual executes only the canonical project's configured argv, at the
// pinned task worktree. The command owns browser startup and loopback policy;
// AIH bounds its process lifetime and accepts only validated local artifacts.
func (c *Controller) captureVisual(ctx context.Context, e config.Effective, task *model.Task, dir string) (*model.VisualEvidence, error) {
	capture := e.Project.VisualCapture
	if capture == nil {
		return nil, &visualCaptureUnavailableError{errors.New("visual capture is not configured for this project")}
	}
	if !visualTaskID.MatchString(task.ID) || !visualRevision.MatchString(task.HeadSHA) || !visualHash.MatchString(e.Hash) {
		return nil, &visualCaptureUnavailableError{errors.New("visual capture needs an exact task head and config hash")}
	}
	sha, err := (gitx.Git{Dir: dir}).SHA(ctx, "HEAD")
	if err != nil || sha != task.HeadSHA {
		return nil, &checkFailure{name: "visual capture", command: "node", err: errors.New("worktree is not at the reviewed head")}
	}
	parent, parentErr := os.Lstat(c.P.Dir)
	if parentErr != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("AIH project state is not a plain directory")
	}
	root := filepath.Join(c.P.Dir, "visual-evidence")
	if err = os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	rootInfo, rootErr := os.Lstat(root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("visual evidence parent is not a plain directory")
	}
	base := filepath.Join(root, task.ID)
	if err = os.Mkdir(base, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	info, statErr := os.Lstat(base)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("visual evidence task path is not a plain directory")
	}
	output := filepath.Join(base, task.HeadSHA+"-"+e.Hash[:16])
	if info, err := os.Lstat(output); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("visual evidence path is not a directory")
		}
		if cached, err := loadSealedVisualEvidence(output, task.ID, task.HeadSHA, e.Hash); err == nil {
			cached.Summary = safety.Portable(cached.Summary, c.P.Dir, dir)
			if task.Evidence != nil && task.Evidence.Visual != nil && !reflect.DeepEqual(*task.Evidence.Visual, *cached) {
				return nil, errors.New("cached visual capture differs from durable review evidence")
			}
			return cached, nil
		}
		if err := quarantineVisualCapture(output); err != nil {
			return nil, &checkFailure{name: "visual capture", command: "node", err: fmt.Errorf("cached artifacts are invalid and cannot be recaptured: %w", err)}
		}
		if c.P.DB != nil {
			_ = c.P.DB.Event(task.ID, task.RunID, "verification", "native", "visual_capture_recaptured", "head="+task.HeadSHA+" config="+e.Hash[:16]+" provenance=quarantined-corrupt-cache")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	temporary, err := os.MkdirTemp(base, ".capture-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temporary)
	runner := filepath.Join(temporary, "aih-visual-runner.mjs")
	if err := os.WriteFile(runner, []byte(visualRunner), 0600); err != nil {
		return nil, err
	}
	release := func() {}
	// Direct helper tests intentionally have no acquired controller snapshot;
	// every live capture reserves the same heavy permit as native checks.
	if c.s != nil {
		var permitErr error
		release, permitErr = c.checkPermit(ctx, task.ID, config.Check{Name: "visual capture", Class: "heavy"})
		if permitErr != nil {
			return nil, permitErr
		}
	}
	defer release()
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(capture.Timeout)*time.Second)
	defer cancel()
	env := append(cleanEnvironment(), "AIH_VISUAL_OUTPUT_DIR="+temporary, "AIH_VISUAL_HEAD="+task.HeadSHA, "AIH_VISUAL_URL="+capture.URL)
	out, err := platform.Run(checkCtx, dir, env, "", "node", runner)
	if err != nil {
		return nil, visualCaptureRunError("node", err, out)
	}
	sha, err = (gitx.Git{Dir: dir}).SHA(ctx, "HEAD")
	if err != nil || sha != task.HeadSHA {
		return nil, errors.New("visual capture changed the reviewed head")
	}
	dirty, err := (gitx.Git{Dir: dir}).Run(ctx, "", "status", "--porcelain")
	if err != nil || dirty != "" {
		return nil, errors.New("visual capture modified source or created unignored files")
	}
	visual, err := loadVisualEvidence(temporary, task.ID, task.HeadSHA, e.Hash)
	if err != nil {
		return nil, &checkFailure{name: "visual capture", command: "node", err: err}
	}
	if err = sealVisualEvidence(temporary, visual); err != nil {
		return nil, err
	}
	if err = os.Rename(temporary, output); err != nil {
		return nil, err
	}
	visual.Summary = safety.Portable(visual.Summary, c.P.Dir, dir)
	return visual, nil
}
