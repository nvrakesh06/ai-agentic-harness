package engine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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
	head := os.Getenv("AIH_VISUAL_HEAD")
	if os.Getenv("AIH_VISUAL_STALE_HEAD") == "1" {
		head = strings.Repeat("0", 40)
	}
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(fmt.Sprintf(`{"head":%q,"summary":"blank page: frontend module 404","artifacts":["desktop.png","network.txt"]}`, head)), 0600)
}

// TestNativeVisualAdapter is a disposable project-side adapter. It deliberately
// binds port zero and publishes only the bounded readiness line required by
// AIH; certificate paths are supplied by the supervisor.
func TestNativeVisualAdapter(t *testing.T) {
	if os.Getenv("AIH_VISUAL_SERVER_HELPER") != "1" {
		return
	}
	cert, err := tls.LoadX509KeyPair(os.Getenv("AIH_VISUAL_TLS_CERT"), os.Getenv("AIH_VISUAL_TLS_KEY"))
	if err != nil {
		os.Exit(2)
	}
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		os.Exit(2)
	}
	defer listener.Close()
	forbidden := os.Getenv("AIH_VISUAL_TEST_FORBIDDEN")
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if os.Getenv("AIH_VISUAL_TEST_NAVIGATION") == "1" {
				http.Redirect(w, r, forbidden+"/navigation", http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><div id="root"></div><img src="https://outside.invalid/pixel.png"><img src="/redirect-pixel"><script>new WebSocket('ws://outside.invalid/socket')</script><script src="/api-client.ts"></script>`))
		case "/api-client.ts":
			http.NotFound(w, r)
		case "/redirect-pixel":
			http.Redirect(w, r, forbidden+"/pixel.png", http.StatusFound)
		case "/redirect-navigation":
			http.Redirect(w, r, forbidden+"/navigation", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})}
	fmt.Printf("%shttps://127.0.0.1:%d\n", visualReadyPrefix, listener.Addr().(*net.TCPAddr).Port)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		os.Exit(2)
	}
}

func TestNativeVisualCapturePinsHeadAndStoresOutsideSource(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AIH_REAL_PLAYWRIGHT") != "1" {
		t.Skip("real Playwright fixture runs on an explicitly provisioned Windows browser host")
	}
	home := os.Getenv("AIH_HOME")
	if module, err := fixturePlaywrightModule(home); err != nil {
		t.Skipf("supervisor Playwright capability unavailable: %v", err)
	} else {
		t.Setenv("AIH_PLAYWRIGHT_MODULE", module)
	}
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
	var forbiddenRequests atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forbiddenRequests.Add(1)
		_, _ = w.Write([]byte("forbidden destination reached"))
	}))
	defer forbidden.Close()
	t.Setenv("AIH_VISUAL_SERVER_HELPER", "1")
	t.Setenv("AIH_VISUAL_TEST_FORBIDDEN", forbidden.URL)
	adapter := []string{os.Args[0], "-test.run=^TestNativeVisualAdapter$"}
	c := &Controller{P: &Project{Home: home, Dir: state}}
	effective := config.Effective{Hash: strings.Repeat("b", 64), Project: config.Project{VisualCapture: &config.VisualCapture{Server: adapter, Timeout: 10}}}
	task := &model.Task{ID: "task-visual", HeadSHA: head}
	if _, err := c.captureVisual(context.Background(), effective, &model.Task{ID: "../outside", HeadSHA: head}, worktree); err == nil {
		t.Fatal("unsafe task ID escaped evidence root")
	}
	visual, err := c.captureVisual(context.Background(), effective, task, worktree)
	if err != nil || visual.Head != head || len(visual.Artifacts) != 2 {
		t.Fatalf("capture failed: %#v %v", visual, err)
	}
	network, err := os.ReadFile(filepath.Join(state, filepath.FromSlash(filepath.Dir(visual.Manifest)), "network.txt"))
	if err != nil || !strings.Contains(string(network), "404 /api-client.ts") || !strings.Contains(string(network), "BLOCKED GET https://outside.invalid") || !strings.Contains(string(network), "BLOCKED WEBSOCKET ws://outside.invalid") || !strings.Contains(string(network), "BLOCKED REDIRECT "+forbidden.URL) {
		t.Fatalf("real blank-page capture omitted failed module evidence: %q %v", network, err)
	}
	if forbiddenRequests.Load() != 0 {
		t.Fatalf("forbidden subresource redirect reached destination %d times", forbiddenRequests.Load())
	}
	t.Setenv("AIH_VISUAL_TEST_NAVIGATION", "1")
	navigation := config.Effective{Hash: strings.Repeat("c", 64), Project: config.Project{VisualCapture: &config.VisualCapture{Server: adapter, Timeout: 10}}}
	if _, err := c.captureVisual(context.Background(), navigation, &model.Task{ID: "task-navigation", HeadSHA: head}, worktree); err == nil {
		t.Fatal("off-origin navigation redirect produced capture evidence")
	}
	if forbiddenRequests.Load() != 0 {
		t.Fatalf("forbidden navigation redirect reached destination %d times", forbiddenRequests.Load())
	}
	// The redirect is a one-capture scenario. Clear it before exercising the
	// normal exact-head cache and recapture path.
	t.Setenv("AIH_VISUAL_TEST_NAVIGATION", "")
	if !strings.HasPrefix(filepath.Join(state, filepath.FromSlash(visual.Manifest)), state) {
		t.Fatal("manifest escaped AIH state")
	}
	if again, err := c.captureVisual(context.Background(), effective, task, worktree); err != nil || again.Manifest != visual.Manifest {
		t.Fatalf("exact-head cache missed: %#v %v", again, err)
	}
	imagePath := filepath.Join(state, filepath.FromSlash(filepath.Dir(visual.Manifest)), "desktop.png")
	originalImage, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	modified := image.NewRGBA(image.Rect(0, 0, 2, 2))
	modified.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err = png.Encode(changed, modified); err != nil {
		t.Fatal(err)
	}
	if err = changed.Close(); err != nil {
		t.Fatal(err)
	}
	if recaptured, err := c.captureVisual(context.Background(), effective, task, worktree); err != nil || recaptured.Manifest != visual.Manifest {
		t.Fatalf("one corrupt-cache recapture was not accepted: %#v %v", recaptured, err)
	}
	if _, err := c.captureVisual(context.Background(), effective, task, worktree); err != nil {
		t.Fatalf("sealed recapture cache was not reused: %v", err)
	}
	if err := os.WriteFile(imagePath, originalImage, 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(state, filepath.FromSlash(visual.Manifest))
	changedManifest := fmt.Sprintf(`{"head":%q,"summary":"altered summary","artifacts":["desktop.png","network.txt"]}`, head)
	if err := os.WriteFile(manifestPath, []byte(changedManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.captureVisual(context.Background(), effective, task, worktree); err == nil {
		t.Fatal("second corrupt cache accepted after bounded recapture")
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

func fixturePlaywrightModule(home string) (string, error) {
	return playwrightModule(home)
}

func TestPlaywrightModuleRequiresResolvedAIHToolsPath(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	toolsModule := filepath.Join(home, "tools", "node_modules", "playwright")
	if err := os.MkdirAll(toolsModule, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_PLAYWRIGHT_MODULE", toolsModule)
	resolved, err := playwrightModule(home)
	if err != nil || resolved == "" {
		t.Fatalf("AIH-owned tools module rejected: %q %v", resolved, err)
	}
	projectModule := filepath.Join(project, "node_modules", "playwright")
	if err = os.MkdirAll(projectModule, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_PLAYWRIGHT_MODULE", projectModule)
	if _, err = playwrightModule(home); err == nil {
		t.Fatal("project-controlled Playwright module accepted")
	}
	symlink := filepath.Join(home, "tools", "linked-playwright")
	if err = os.Symlink(projectModule, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("AIH_PLAYWRIGHT_MODULE", symlink)
	if _, err = playwrightModule(home); err == nil {
		t.Fatal("Playwright symlink escaping AIH tools accepted")
	}
	toolsRoot := filepath.Join(home, "tools")
	if err = os.RemoveAll(toolsRoot); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(project, toolsRoot); err != nil {
		t.Skipf("tools-root symlink unavailable: %v", err)
	}
	t.Setenv("AIH_PLAYWRIGHT_MODULE", filepath.Join(toolsRoot, "node_modules", "playwright"))
	if _, err = playwrightModule(home); err == nil {
		t.Fatal("AIH tools root linked into a project worktree accepted")
	}
}

func TestVisualRunnerOwnsLoopbackAndProfilePolicy(t *testing.T) {
	for _, want := range []string{"channel: 'chrome'", "viewport: { width: 1280, height: 720 }", "serviceWorkers: 'block'", "await context.route", "route.fetch({ maxRedirects: 0 })", "BLOCKED REDIRECT", "await context.routeWebSocket", "url.origin !== origin", "route.abort('blockedbyclient')", "ws.close()", "await ws.connectToServer()", "context.newPage", "browser.close"} {
		if !strings.Contains(visualRunner, want) {
			t.Fatalf("AIH runner omitted required policy %q", want)
		}
	}
}

// A listener that did not receive this capture's certificate cannot be mistaken
// for the adapter, even if it presents a plausible application page.
func TestVisualGatewayRejectsStaleListenerBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	stale := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("plausible stale application"))
	}))
	defer stale.Close()
	target, err := url.Parse(stale.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _, cert, err := visualCertificate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = startVisualGateway(context.Background(), target, cert); err == nil {
		t.Fatal("stale listener with another certificate passed pinned gateway setup")
	}
	if requests.Load() != 0 {
		t.Fatalf("stale listener received HTTP after rejected TLS handshake: %d", requests.Load())
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

func TestVisualCaptureErrorsSeparateUnavailableToolFromSourceFailure(t *testing.T) {
	c := &Controller{P: &Project{Dir: t.TempDir()}}
	task := &model.Task{ID: "task-visual", HeadSHA: strings.Repeat("a", 40)}
	effective := config.Effective{Hash: strings.Repeat("b", 64)}
	_, err := c.captureVisual(context.Background(), effective, task, t.TempDir())
	var unavailable *visualCaptureUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("missing capture configuration was not typed unavailable: %v", err)
	}

	err = visualCaptureRunError("aih-capture-tool-that-does-not-exist", exec.ErrNotFound, "")
	if !errors.As(err, &unavailable) {
		t.Fatalf("missing capture tool was not typed unavailable: %v", err)
	}
	for _, stage := range []string{"playwright-module", "browser-launch", "playwright-api"} {
		err = visualCaptureRunError("node", errors.New("exit status 78"), "AIH_VISUAL_UNAVAILABLE:"+stage)
		if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), stage) {
			t.Fatalf("supervisor %s failure entered source FIX: %v", stage, err)
		}
	}
	err = visualCaptureRunError("capture-review", errors.New("exit status 1"), "fixture source failure")
	var source *checkFailure
	if !errors.As(err, &source) {
		t.Fatalf("capture source failure was not routed as native evidence: %v", err)
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
	write("manifest.json", fmt.Sprintf(`{"head":%q,"summary":"blank page; frontend module returned 404","artifacts":["frame.png","network.txt"]}`, head))
	visual, err := loadVisualEvidence(dir, "task-1", head, cfg)
	if err != nil || len(visual.Artifacts) != 2 || visual.Head != head || !strings.HasPrefix(visual.Manifest, "visual-evidence/task-1/") {
		t.Fatalf("valid visual capture rejected: %#v %v", visual, err)
	}
	portable, err := json.Marshal(visual)
	if err != nil || strings.Contains(string(portable), "GET /api-client.ts 404") {
		t.Fatalf("portable visual evidence included local artifact bytes: %q %v", portable, err)
	}
	write("manifest.json", fmt.Sprintf(`{"head":%q,"summary":"bad","artifacts":["../secret.png"]}`, head))
	if _, err := loadVisualEvidence(dir, "task-1", head, cfg); err == nil {
		t.Fatal("traversal artifact accepted")
	}
	write("manifest.json", fmt.Sprintf(`{"head":%q,"summary":"bad","artifacts":["frame.png","network.txt"]}`, head))
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
