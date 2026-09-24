package engine

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

const visualFileLimit = 8 << 20
const visualReadyPrefix = "AIH_VISUAL_READY "

// The runner is deliberately AIH-owned. It creates a fresh browser context,
// pins its viewport/channel, and aborts every request whose origin differs
// from the configured loopback origin, including redirects, subresources, and
// WebSockets. Project configuration supplies no browser arguments or URL.
const visualRunner = `
import { writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
let chromium;
try {
  ({ chromium } = require(process.env.AIH_VISUAL_PLAYWRIGHT_MODULE));
} catch {
  console.error('AIH_VISUAL_UNAVAILABLE:playwright-module');
  process.exit(78);
}
const output = process.env.AIH_VISUAL_OUTPUT_DIR;
const head = process.env.AIH_VISUAL_HEAD;
const target = process.env.AIH_VISUAL_URL;
const origin = new URL(target).origin;
const network = [];
let browser;
try {
  browser = await chromium.launch({ channel: 'chrome', headless: true });
} catch {
  console.error('AIH_VISUAL_UNAVAILABLE:browser-launch');
  process.exit(78);
}
let context;
try {
  context = await browser.newContext({ viewport: { width: 1280, height: 720 }, serviceWorkers: 'block' });
  if (typeof context.route !== 'function' || typeof context.routeWebSocket !== 'function') throw new Error('missing route API');
} catch {
  await browser.close();
  console.error('AIH_VISUAL_UNAVAILABLE:playwright-api');
  process.exit(78);
}
await context.route('**/*', async route => {
  const request = route.request();
  const url = new URL(request.url());
  if (url.origin !== origin) { network.push('BLOCKED ' + request.method() + ' ' + url.origin); return route.abort('blockedbyclient'); }
  const response = await route.fetch({ maxRedirects: 0 });
  const location = response.headers()['location'];
  if (location && new URL(location, url).origin !== origin) { network.push('BLOCKED REDIRECT ' + new URL(location, url).origin); return route.abort('blockedbyclient'); }
  return route.fulfill({ response });
});
await context.routeWebSocket('**/*', async ws => {
  const url = new URL(ws.url());
  if (url.origin !== origin.replace(/^http/, 'ws')) { network.push('BLOCKED WEBSOCKET ' + url.origin); return ws.close(); }
  await ws.connectToServer();
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
	for _, stage := range []string{"playwright-module", "browser-launch", "playwright-api"} {
		if strings.Contains(output, "AIH_VISUAL_UNAVAILABLE:"+stage) {
			return &visualCaptureUnavailableError{fmt.Errorf("supervisor browser capability failed at %s", stage)}
		}
	}
	return &checkFailure{name: "visual capture", command: filepath.Base(command), err: err, output: short(safety.Redact(output), 2000)}
}

// playwrightModule is an explicit machine capability. The resolved module must
// live below AIH_HOME/tools, never in a target worktree or behind a link that
// escapes that supervisor-owned directory.
func playwrightModule(home string) (string, error) {
	module := os.Getenv("AIH_PLAYWRIGHT_MODULE")
	if module == "" || !filepath.IsAbs(module) || home == "" {
		return "", &visualCaptureUnavailableError{errors.New("AIH_PLAYWRIGHT_MODULE must name an absolute module below AIH_HOME/tools")}
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", &visualCaptureUnavailableError{errors.New("AIH home is unavailable")}
	}
	expectedTools := filepath.Join(resolvedHome, "tools")
	toolsInfo, err := os.Lstat(expectedTools)
	if err != nil || !toolsInfo.IsDir() || toolsInfo.Mode()&os.ModeSymlink != 0 {
		return "", &visualCaptureUnavailableError{errors.New("AIH tools directory is unavailable")}
	}
	tools, err := filepath.EvalSymlinks(expectedTools)
	if err != nil || filepath.Clean(tools) != filepath.Clean(expectedTools) {
		return "", &visualCaptureUnavailableError{errors.New("AIH tools directory must be a real child of AIH_HOME")}
	}
	module, err = filepath.EvalSymlinks(module)
	if err != nil {
		return "", &visualCaptureUnavailableError{errors.New("AIH_PLAYWRIGHT_MODULE is not an available Playwright module")}
	}
	info, err := os.Stat(module)
	if err != nil || !info.IsDir() || filepath.Base(module) != "playwright" || !pathWithin(tools, module) {
		return "", &visualCaptureUnavailableError{errors.New("AIH_PLAYWRIGHT_MODULE must resolve below AIH_HOME/tools/playwright")}
	}
	return module, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// visualCertificate creates capture-only credentials. They live in the AIH
// temporary directory, never in the reviewed worktree, and are discarded with
// the capture. The adapter receives their paths solely to terminate TLS.
func visualCertificate(dir string) (certPath, keyPath string, cert *x509.Certificate, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", "", nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "aih-visual-capture"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"aih-visual-capture"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", nil, err
	}
	certPath, keyPath = filepath.Join(dir, "adapter-cert.pem"), filepath.Join(dir, "adapter-key.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return "", "", nil, err
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return "", "", nil, err
	}
	cert, err = x509.ParseCertificate(der)
	return certPath, keyPath, cert, err
}

func adapterReady(ctx context.Context, process *platform.ManagedProcess) (*url.URL, error) {
	lines := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(io.LimitReader(process.Stdout, 4096))
		scanner.Buffer(make([]byte, 256), 4096)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, visualReadyPrefix) {
				lines <- strings.TrimPrefix(line, visualReadyPrefix)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errs <- err
		} else {
			errs <- errors.New("server adapter exited before bounded readiness")
		}
	}()
	select {
	case line := <-lines:
		target, err := url.Parse(line)
		if err != nil || target.Scheme != "https" || target.Hostname() != "127.0.0.1" || target.Port() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || target.Path != "" {
			return nil, errors.New("server adapter readiness must be https://127.0.0.1:<port>")
		}
		port, err := strconv.Atoi(target.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("server adapter readiness must use a valid loopback port")
		}
		return target, nil
	case err := <-errs:
		return nil, err
	case <-ctx.Done():
		process.Close()
		return nil, ctx.Err()
	}
}

type visualGateway struct {
	url    string
	server *http.Server
	listen net.Listener
	once   sync.Once
}

func (g *visualGateway) Close() { g.once.Do(func() { _ = g.server.Close(); _ = g.listen.Close() }) }

// startVisualGateway authenticates the adapter once through a fresh pinned TLS
// transport, then gives the browser an AIH-owned HTTP loopback origin. No
// system proxy or redirect-following client participates in either connection.
func startVisualGateway(ctx context.Context, target *url.URL, cert *x509.Certificate) (*visualGateway, error) {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "aih-visual-capture", MinVersion: tls.VersionTLS12}}
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(probe)
	if err != nil {
		return nil, fmt.Errorf("server adapter pinned TLS handshake: %w", err)
	}
	_ = response.Body.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "AIH adapter connection failed", http.StatusBadGateway)
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
	gateway := &visualGateway{url: "http://" + listener.Addr().String() + "/", server: server, listen: listener}
	go func() { _ = server.Serve(listener) }()
	return gateway, nil
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
	playwright, err := playwrightModule(c.P.Home)
	if err != nil {
		return nil, err
	}
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
	certPath, keyPath, cert, err := visualCertificate(temporary)
	if err != nil {
		return nil, err
	}
	adapterEnv := append(cleanEnvironment(), "AIH_VISUAL_TLS_CERT="+certPath, "AIH_VISUAL_TLS_KEY="+keyPath)
	adapter, err := platform.StartManaged(checkCtx, dir, adapterEnv, capture.Server[0], capture.Server[1:]...)
	if err != nil {
		return nil, visualCaptureRunError(capture.Server[0], err, "")
	}
	defer adapter.Close()
	target, err := adapterReady(checkCtx, adapter)
	if err != nil {
		return nil, visualCaptureRunError(capture.Server[0], err, "")
	}
	gateway, err := startVisualGateway(checkCtx, target, cert)
	if err != nil {
		return nil, visualCaptureRunError(capture.Server[0], err, "")
	}
	defer gateway.Close()
	env := append(cleanEnvironment(), "AIH_VISUAL_OUTPUT_DIR="+temporary, "AIH_VISUAL_HEAD="+task.HeadSHA, "AIH_VISUAL_URL="+gateway.url, "AIH_VISUAL_PLAYWRIGHT_MODULE="+playwright)
	out, err := platform.Run(checkCtx, dir, env, "", "node", runner)
	if err != nil {
		return nil, visualCaptureRunError("node", err, out)
	}
	gateway.Close()
	adapter.Close()
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
