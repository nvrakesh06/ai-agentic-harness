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
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
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

func TestVisualPrepareHelper(t *testing.T) {
	if os.Getenv("AIH_VISUAL_PREPARE_HELPER") != "1" {
		return
	}
	appendVisualEnvironmentMarker("prepare")
	if marker := os.Getenv("AIH_VISUAL_PREPARE_MARKER"); marker != "" {
		if os.Getenv("AIH_VISUAL_PREPARE_COUNT") == "1" {
			previous, _ := os.ReadFile(marker)
			_ = os.WriteFile(marker, append(previous, '1'), 0600)
		} else {
			_ = os.WriteFile(marker, []byte(os.Getenv("AIH_VISUAL_PREPARE_VALUE")+"\n"), 0600)
		}
	}
	if os.Getenv("AIH_VISUAL_PREPARE_WAIT") == "1" {
		select {}
	}
}

func appendVisualEnvironmentMarker(role string) {
	appendVisualEnvironmentMarkerValue(role, os.Getenv("PLAYWRIGHT_BROWSERS_PATH"))
}

func appendVisualEnvironmentMarkerValue(role, value string) {
	marker := os.Getenv("AIH_VISUAL_ENV_MARKER")
	if marker == "" {
		return
	}
	previous, _ := os.ReadFile(marker)
	_ = os.WriteFile(marker, append(previous, []byte(role+"="+value+"\n")...), 0600)
}

func TestVisualEnvironmentBindsPrivatePlaywrightBrowserCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "visual-cache")
	hostileBrowserCache := filepath.Join(t.TempDir(), "shared-playwright")
	t.Setenv("PLAYWRIGHT_BROWSERS_PATH", hostileBrowserCache)
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "host-local-app-data"))
	values := map[string]string{}
	counts := map[string]int{}
	for _, item := range visualEnvironment(cache) {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToUpper(parts[0])
		values[key] = parts[1]
		counts[key]++
	}
	want := filepath.Join(cache, "playwright-browsers")
	if values["PLAYWRIGHT_BROWSERS_PATH"] != want || counts["PLAYWRIGHT_BROWSERS_PATH"] != 1 {
		t.Fatalf("PLAYWRIGHT_BROWSERS_PATH = %q (%d entries), want one private cache path %q", values["PLAYWRIGHT_BROWSERS_PATH"], counts["PLAYWRIGHT_BROWSERS_PATH"], want)
	}
	if values["AIH_VISUAL_CACHE_DIR"] != cache || values["XDG_CACHE_HOME"] != cache {
		t.Fatalf("visual cache bindings = %#v, want %q", values, cache)
	}
}

func visualCheckoutFixture(t *testing.T) (*Controller, *model.Task, string, string, func(string, ...string) string) {
	t.Helper()
	state, source, control := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "control.git")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(source, "init")
	run(source, "config", "user.email", "test@example.invalid")
	run(source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("tracked"), 0600); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "tracked.txt")
	run(source, "commit", "-m", "base")
	head := run(source, "rev-parse", "HEAD")
	cmd := exec.Command("git", "init", "--bare", control)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init control: %v %s", err, out)
	}
	run(source, "remote", "add", "control", control)
	run(source, "push", "control", "HEAD:refs/heads/aih/task")
	writer := filepath.Join(t.TempDir(), "writer")
	cmd = exec.Command("git", "clone", "-b", "aih/task", control, writer)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone writer: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(writer, ".gitignore"), []byte("node_modules/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(writer, "node_modules"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(writer, "node_modules", "fake-renderer"), []byte("not invoked"), 0600); err != nil {
		t.Fatal(err)
	}
	return &Controller{P: &Project{Dir: state, Git: gitx.Git{Dir: control}}}, &model.Task{ID: "task-visual", HeadSHA: head}, writer, source, func(dir string, args ...string) string { return run(dir, args...) }
}

func TestVisualPrepareUsesFreshDetachedCheckoutNotWriterRuntime(t *testing.T) {
	c, task, writer, _, _ := visualCheckoutFixture(t)
	checkout, err := c.newVisualCheckout(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "prepare-marker")
	t.Setenv("AIH_VISUAL_PREPARE_HELPER", "1")
	t.Setenv("AIH_VISUAL_PREPARE_MARKER", marker)
	t.Setenv("AIH_VISUAL_PREPARE_VALUE", checkout.path)
	cache := t.TempDir()
	// Capture targets are all handled later by one browser invocation. Prepare
	// remains one operation even when the capture declares multiple targets.
	targets := (&config.VisualCapture{Targets: []config.VisualCaptureTarget{{ID: "one", Path: "/", Width: 2, Height: 2}, {ID: "two", Path: "/two", Width: 2, Height: 2}}}).CaptureTargets()
	if len(targets) != 2 {
		t.Fatal("fixture did not declare multiple visual targets")
	}
	if _, err = platform.Run(context.Background(), checkout.path, visualEnvironment(cache), "", os.Args[0], "-test.run=^TestVisualPrepareHelper$"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(got)) != checkout.path {
		t.Fatalf("prepare ran outside detached checkout: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(writer, "node_modules", "fake-renderer")); err != nil || string(got) != "not invoked" {
		t.Fatalf("ignored writer runtime was invoked or changed: %q %v", got, err)
	}
	checkoutPath := checkout.path
	if err = checkout.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(checkoutPath); !os.IsNotExist(err) {
		t.Fatalf("AIH-owned detached checkout survived cleanup: %v", err)
	}
}

func TestVisualCheckoutCleansUpAfterCanceledPrepare(t *testing.T) {
	c, task, _, _, _ := visualCheckoutFixture(t)
	checkout, err := c.newVisualCheckout(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	checkoutPath := checkout.path
	t.Setenv("AIH_VISUAL_PREPARE_HELPER", "1")
	t.Setenv("AIH_VISUAL_PREPARE_WAIT", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = platform.Run(ctx, checkout.path, visualEnvironment(t.TempDir()), "", os.Args[0], "-test.run=^TestVisualPrepareHelper$"); err == nil {
		t.Fatal("canceled prepare completed")
	}
	if err = checkout.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(checkoutPath); !os.IsNotExist(err) {
		t.Fatalf("canceled prepare left its detached checkout behind: %v", err)
	}
}

func TestVisualCheckoutDoesNotReusePreparedHeadAcrossRestart(t *testing.T) {
	c, task, _, source, run := visualCheckoutFixture(t)
	first, err := c.newVisualCheckout(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := first.path
	if err = first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("head-b"), 0600); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "tracked.txt")
	run(source, "commit", "-m", "head b")
	run(source, "push", "control", "HEAD:refs/heads/aih/task")
	task.HeadSHA = run(source, "rev-parse", "HEAD")
	second, err := c.newVisualCheckout(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if second.path == firstPath {
		t.Fatal("head B reused head A's prepared checkout path")
	}
	if sha, err := (gitx.Git{Dir: second.path}).SHA(context.Background(), "HEAD"); err != nil || sha != task.HeadSHA {
		t.Fatalf("head B checkout has the wrong revision: %s %v", sha, err)
	}
	if err = second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyVisualSealIsQuarantinedForDetachedCheckoutRecapture(t *testing.T) {
	dir := t.TempDir()
	head, cfg := strings.Repeat("a", 40), strings.Repeat("b", 64)
	if err := writeVisualPNG(filepath.Join(dir, "desktop.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network.txt"), []byte("desktop GET 200 /"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(fmt.Sprintf(`{"head":%q,"summary":"legacy cache","artifacts":["desktop.png","network.txt"]}`, head)), 0600); err != nil {
		t.Fatal(err)
	}
	evidence, err := loadVisualEvidence(dir, "task-visual", head, cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(evidence) // schema used before detached checkout provenance
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "capture-seal.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadSealedVisualEvidence(dir, "task-visual", head, cfg); !errors.Is(err, errLegacyVisualSeal) {
		t.Fatalf("legacy seal was reusable instead of requiring recapture: %v", err)
	}
	if err = quarantineVisualCapture(dir); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(dir + ".corrupt"); err != nil {
		t.Fatalf("legacy seal was not safely quarantined: %v", err)
	}
}

func TestCaptureVisualDoesNotRunIgnoredWriterRenderer(t *testing.T) {
	c, task, writer, _, _ := visualCheckoutFixture(t)
	home := t.TempDir()
	module := filepath.Join(home, "tools", "playwright")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	c.P.Home = home
	t.Setenv("AIH_PLAYWRIGHT_MODULE", module)
	prepareMarker := filepath.Join(t.TempDir(), "prepare-count")
	tripwire := filepath.Join(t.TempDir(), "writer-renderer-invoked")
	t.Setenv("AIH_VISUAL_PREPARE_HELPER", "1")
	t.Setenv("AIH_VISUAL_PREPARE_MARKER", prepareMarker)
	t.Setenv("AIH_VISUAL_PREPARE_COUNT", "1")
	t.Setenv("AIH_VISUAL_SERVER_HELPER", "1")
	t.Setenv("AIH_VISUAL_WRITER_TRIPWIRE", tripwire)
	envMarker := filepath.Join(t.TempDir(), "playwright-browser-paths")
	t.Setenv("AIH_VISUAL_ENV_MARKER", envMarker)
	t.Setenv("PLAYWRIGHT_BROWSERS_PATH", filepath.Join(t.TempDir(), "host-shared-playwright"))
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "host-local-app-data"))
	originalBrowserRun := runVisualBrowser
	runVisualBrowser = func(_ context.Context, dir string, env []string, _ string) (string, error) {
		values := map[string]string{}
		for _, item := range env {
			parts := strings.SplitN(item, "=", 2)
			if len(parts) == 2 {
				values[parts[0]] = parts[1]
			}
		}
		if dir == writer {
			return "", errors.New("browser runner used writer worktree")
		}
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "fake-renderer")); err == nil {
			return "", errors.New("browser runner observed writer fake renderer")
		}
		browserCache := values["PLAYWRIGHT_BROWSERS_PATH"]
		if browserCache == "" {
			return "", errors.New("browser runner missing Playwright browser cache")
		}
		if err := os.MkdirAll(browserCache, 0700); err != nil {
			return "", err
		}
		appendVisualEnvironmentMarkerValue("browser", browserCache)
		var targets []config.VisualCaptureTarget
		if err := json.Unmarshal([]byte(values["AIH_VISUAL_TARGETS"]), &targets); err != nil {
			return "", err
		}
		artifacts := []string{"network.txt"}
		manifestTargets := make([]visualManifestTarget, 0, len(targets))
		for _, target := range targets {
			name := target.ID + ".png"
			if err := writeVisualPNG(filepath.Join(values["AIH_VISUAL_OUTPUT_DIR"], name)); err != nil {
				return "", err
			}
			artifacts = append([]string{name}, artifacts...)
			manifestTargets = append(manifestTargets, visualManifestTarget{ID: target.ID, Path: target.Path, Width: target.Width, Height: target.Height, Screenshot: name})
		}
		if err := os.WriteFile(filepath.Join(values["AIH_VISUAL_OUTPUT_DIR"], "network.txt"), []byte("capture GET 200 /"), 0600); err != nil {
			return "", err
		}
		manifest, err := json.Marshal(visualManifest{Head: values["AIH_VISUAL_HEAD"], Summary: "test capture", Artifacts: artifacts, Targets: manifestTargets})
		if err != nil {
			return "", err
		}
		return "", os.WriteFile(filepath.Join(values["AIH_VISUAL_OUTPUT_DIR"], "manifest.json"), manifest, 0600)
	}
	defer func() { runVisualBrowser = originalBrowserRun }()
	targets := []config.VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 2, Height: 2}, {ID: "detail", Path: "/detail", Width: 2, Height: 2}}
	effective := config.Effective{Hash: strings.Repeat("b", 64), Project: config.Project{VisualCapture: &config.VisualCapture{Prepare: []string{os.Args[0], "-test.run=^TestVisualPrepareHelper$"}, PrepareTimeout: 5, Server: []string{os.Args[0], "-test.run=^TestNativeVisualAdapter$"}, Timeout: 10, Targets: targets}}}
	visual, err := c.captureVisual(context.Background(), effective, task, writer)
	if err != nil {
		t.Fatal(err)
	}
	if visual == nil || len(visual.Artifacts) != 3 {
		t.Fatalf("full capture did not produce every target: %#v", visual)
	}
	evidenceDir := filepath.Join(c.P.Dir, filepath.FromSlash(filepath.Dir(visual.Manifest)))
	for _, name := range []string{"adapter-cert.pem", "adapter-key.pem", "aih-visual-runner.mjs"} {
		if _, err := os.Stat(filepath.Join(evidenceDir, name)); !os.IsNotExist(err) {
			t.Fatalf("temporary visual capture file was retained as evidence: %s (%v)", name, err)
		}
	}
	if got, err := os.ReadFile(prepareMarker); err != nil || string(got) != "1" {
		t.Fatalf("prepare did not run exactly once for multi-target capture: %q %v", got, err)
	}
	if _, err := os.Stat(tripwire); !os.IsNotExist(err) {
		t.Fatalf("ignored writer renderer was invoked: %v", err)
	}
	paths, err := os.ReadFile(envMarker)
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(paths)), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			bound[parts[0]] = parts[1]
		}
	}
	wantBrowserCache := bound["prepare"]
	if wantBrowserCache == "" || bound["server"] != wantBrowserCache || bound["browser"] != wantBrowserCache || !strings.HasSuffix(wantBrowserCache, filepath.Join("cache", "playwright-browsers")) {
		t.Fatalf("capture environment did not use one private Playwright cache: %q", paths)
	}
	if _, err := os.Stat(wantBrowserCache); !os.IsNotExist(err) {
		t.Fatalf("browser cache survived capture cleanup: %v", err)
	}
}

// TestNativeVisualAdapter is a disposable project-side adapter. It deliberately
// binds port zero and publishes only the bounded readiness line required by
// AIH; certificate paths are supplied by the supervisor.
func TestNativeVisualAdapter(t *testing.T) {
	if os.Getenv("AIH_VISUAL_SERVER_HELPER") != "1" {
		return
	}
	appendVisualEnvironmentMarker("server")
	if tripwire := os.Getenv("AIH_VISUAL_WRITER_TRIPWIRE"); tripwire != "" {
		if _, err := os.Stat(filepath.Join("node_modules", "fake-renderer")); err == nil {
			_ = os.WriteFile(tripwire, []byte("writer fake renderer was visible"), 0600)
			os.Exit(3)
		}
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
			_, _ = w.Write([]byte(`<!doctype html><main><h1>AIH native visual capture</h1><p>loopback adapter fixture</p></main><img src="https://outside.invalid/pixel.png"><img src="/redirect-pixel"><script>new WebSocket('ws://outside.invalid/socket')</script><script src="/api-client.ts"></script>`))
		case "/detail":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><style>html,body{margin:0;width:100%;height:100%;background:rgb(17,34,51)}</style><main>detail state</main>`))
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
	// Live captures create their detached checkout from the fetched control
	// repository, not the writer/source directory. Keep this native fixture on
	// that same provenance path so the pinned commit is resolvable where the
	// production checkout is created.
	control := filepath.Join(t.TempDir(), "control.git")
	cmd := exec.Command("git", "clone", "--bare", worktree, control)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone control: %v %s", err, out)
	}
	if got, err := (gitx.Git{Dir: control}).SHA(context.Background(), head); err != nil || got != head {
		t.Fatalf("control repository does not contain pinned head %q: got %q, err %v", head, got, err)
	}
	var forbiddenRequests atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forbiddenRequests.Add(1)
		_, _ = w.Write([]byte("forbidden destination reached"))
	}))
	defer forbidden.Close()
	t.Setenv("AIH_VISUAL_SERVER_HELPER", "1")
	t.Setenv("AIH_VISUAL_TEST_FORBIDDEN", forbidden.URL)
	adapter := []string{os.Args[0], "-test.run=^TestNativeVisualAdapter$"}
	c := &Controller{P: &Project{Home: home, Dir: state, Git: gitx.Git{Dir: control}}}
	targets := []config.VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 1280, Height: 720}, {ID: "detail", Path: "/detail", Width: 640, Height: 480}}
	effective := config.Effective{Hash: strings.Repeat("b", 64), Project: config.Project{VisualCapture: &config.VisualCapture{Server: adapter, Timeout: 10, Targets: targets}}}
	task := &model.Task{ID: "task-visual", HeadSHA: head}
	if _, err := c.captureVisual(context.Background(), effective, &model.Task{ID: "../outside", HeadSHA: head}, worktree); err == nil {
		t.Fatal("unsafe task ID escaped evidence root")
	}
	visual, err := c.captureVisual(context.Background(), effective, task, worktree)
	if err != nil || visual.Head != head || len(visual.Artifacts) != 3 {
		t.Fatalf("capture failed: %#v %v", visual, err)
	}
	manifest, err := os.ReadFile(filepath.Join(state, filepath.FromSlash(visual.Manifest)))
	if err != nil || !strings.Contains(string(manifest), `"id":"detail"`) || !strings.Contains(string(manifest), `"screenshot":"detail.png"`) {
		t.Fatalf("multi-target manifest missing detail mapping: %q %v", manifest, err)
	}
	if _, err := os.Stat(filepath.Join(state, filepath.FromSlash(filepath.Dir(visual.Manifest)), "detail.png")); err != nil {
		t.Fatalf("multi-target screenshot missing: %v", err)
	}
	detailFile, err := os.Open(filepath.Join(state, filepath.FromSlash(filepath.Dir(visual.Manifest)), "detail.png"))
	if err != nil {
		t.Fatal(err)
	}
	detailImage, _, err := image.Decode(detailFile)
	closeErr := detailFile.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("detail screenshot decode: %v %v", err, closeErr)
	}
	desktopFile, err := os.Open(filepath.Join(state, filepath.FromSlash(filepath.Dir(visual.Manifest)), "desktop.png"))
	if err != nil {
		t.Fatal(err)
	}
	desktopImage, _, err := image.Decode(desktopFile)
	closeErr = desktopFile.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("desktop screenshot decode: %v %v", err, closeErr)
	}
	detailPixel := color.RGBAModel.Convert(detailImage.At(10, 10)).(color.RGBA)
	desktopPixel := color.RGBAModel.Convert(desktopImage.At(10, 10)).(color.RGBA)
	if desktopPixel == detailPixel || detailPixel.R > 64 || detailPixel.G > 64 || detailPixel.B > 64 {
		t.Fatalf("detail screenshot did not render its distinct state: desktop=%#v detail=%#v", desktopPixel, detailPixel)
	}
	network, err := os.ReadFile(filepath.Join(state, filepath.FromSlash(filepath.Dir(visual.Manifest)), "network.txt"))
	if err != nil || !strings.Contains(string(network), "404 /api-client.ts") || !strings.Contains(string(network), "detail GET 200 /detail") || !strings.Contains(string(network), "BLOCKED GET https://outside.invalid") || !strings.Contains(string(network), "BLOCKED WEBSOCKET ws://outside.invalid") || !strings.Contains(string(network), "BLOCKED REDIRECT "+forbidden.URL) {
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

// TestNativeVisualCaptureReattestsIdenticalTree runs the production browser
// runner against the disposable TLS adapter. It is opt-in because it needs a
// locally provisioned Chrome channel and the supervisor-owned Playwright
// module; ordinary tests continue to cover the same protocol with fixtures.
func TestNativeVisualCaptureReattestsIdenticalTree(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AIH_REAL_PLAYWRIGHT") != "1" {
		t.Skip("real Playwright fixture runs on an explicitly provisioned Windows browser host")
	}
	home, err := config.Home("")
	if err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(home, "tools", "node_modules", "playwright")
	t.Setenv("AIH_PLAYWRIGHT_MODULE", module)
	module, err = fixturePlaywrightModule(home)
	if err != nil {
		t.Skipf("supervisor Playwright capability unavailable: %v", err)
	}
	t.Setenv("AIH_VISUAL_SERVER_HELPER", "1")

	c, task, _, source, run := visualCheckoutFixture(t)
	c.P.Home = home
	targets := []config.VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 640, Height: 360}}
	effective := config.Effective{Hash: strings.Repeat("b", 64), Project: config.Project{VisualCapture: &config.VisualCapture{
		Server:       []string{os.Args[0], "-test.run=^TestNativeVisualAdapter$"},
		Timeout:      30,
		Targets:      targets,
		InputClosure: &config.VisualInputClosure{Version: 1, Runtime: "chrome-playwright", Targets: []config.VisualInputClosureTarget{{ID: "desktop"}}},
	}}}
	first, err := c.captureVisual(context.Background(), effective, task, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceHead != task.HeadSHA || first.Closure == "" || first.Runtime == "" || first.ReuseReason != "" {
		t.Fatalf("fresh capture provenance = %#v", first)
	}
	firstDir := filepath.Join(c.P.Dir, filepath.FromSlash(filepath.Dir(first.Manifest)))
	firstImage, err := os.Open(filepath.Join(firstDir, "desktop.png"))
	if err != nil {
		t.Fatal(err)
	}
	firstConfig, _, decodeErr := image.DecodeConfig(firstImage)
	closeErr := firstImage.Close()
	if decodeErr != nil || closeErr != nil || firstConfig.Width != 640 || firstConfig.Height != 360 {
		t.Fatalf("production runner did not create a 640x360 screenshot: %#v %v %v", firstConfig, decodeErr, closeErr)
	}
	if _, _, err = loadVisualSeal(firstDir, task.ID, task.HeadSHA, effective.Hash, targets); err != nil {
		t.Fatalf("fresh capture was not sealed: %v", err)
	}
	for _, name := range []string{"adapter-cert.pem", "adapter-key.pem", "aih-visual-runner.mjs"} {
		if _, err := os.Stat(filepath.Join(firstDir, name)); !os.IsNotExist(err) {
			t.Fatalf("temporary visual capture file was retained as evidence: %s (%v)", name, err)
		}
	}

	// This changes commit identity but preserves the complete committed tree.
	run(source, "commit", "--allow-empty", "-m", "history-only sync")
	secondHead := run(source, "rev-parse", "HEAD")
	run(source, "push", "control", "HEAD:refs/heads/aih/task")
	task.HeadSHA = secondHead
	task.Evidence = &model.Evidence{
		Reviews: map[string]string{"designer": "review from the source head"},
		ReviewDispositions: map[string]model.ReviewDisposition{
			"designer": {Disposition: "completed", SourceHead: first.Head, Runtime: "native"},
		},
	}
	beforeReviews, err := json.Marshal(task.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.captureVisual(context.Background(), effective, task, source)
	if err != nil {
		t.Fatal(err)
	}
	if second.Head != secondHead || second.SourceHead != first.Head || second.Closure != first.Closure || second.Runtime != first.Runtime || second.ReuseReason != "identical declared visual input closure" {
		t.Fatalf("identical-tree reattestation provenance = %#v", second)
	}
	afterReviews, err := json.Marshal(task.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterReviews) != string(beforeReviews) {
		t.Fatalf("artifact reattestation modified or copied review disposition: before=%s after=%s", beforeReviews, afterReviews)
	}
	secondDir := filepath.Join(c.P.Dir, filepath.FromSlash(filepath.Dir(second.Manifest)))
	seal, _, err := loadVisualSeal(secondDir, task.ID, secondHead, effective.Hash, targets)
	if err != nil || seal.Closure == nil || seal.Closure.SourceHead != first.Head || seal.Evidence.SourceHead != first.Head {
		t.Fatalf("reattested capture did not retain source-head provenance: %#v %v", seal, err)
	}
	if root := os.Getenv("AIH_LIVE_VISUAL_ARTIFACT_ROOT"); root != "" {
		destination := filepath.Join(root, "visual-evidence")
		if err := os.CopyFS(destination, os.DirFS(filepath.Join(c.P.Dir, "visual-evidence"))); err != nil {
			t.Fatalf("retain live visual artifacts: %v", err)
		}
		t.Logf("retained live visual artifacts at %s", destination)
	}

	// A tracked-file change produces a different full-tree receipt and cannot
	// reuse the sealed screenshot, even though target/config/runtime match.
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("changed tree"), 0600); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "tracked.txt")
	run(source, "commit", "-m", "tracked change")
	thirdHead := run(source, "rev-parse", "HEAD")
	run(source, "push", "control", "HEAD:refs/heads/aih/task")
	runtimeIdentity, err := visualRuntimeIdentity(context.Background(), module)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := visualInputClosure(context.Background(), source, effective, runtimeIdentity, thirdHead)
	if err != nil {
		t.Fatal(err)
	}
	thirdOutput := filepath.Join(c.P.Dir, "visual-evidence", task.ID, thirdHead+"-"+effective.Hash[:16])
	if _, ok, err := c.reattestVisualEvidence(context.Background(), filepath.Join(c.P.Dir, "visual-evidence", task.ID), thirdOutput, &model.Task{ID: task.ID, HeadSHA: thirdHead}, effective, targets, closure); err != nil || ok {
		t.Fatalf("changed full tree reused a sealed capture: ok=%t err=%v", ok, err)
	}
	if _, err := os.Stat(thirdOutput); !os.IsNotExist(err) {
		t.Fatalf("changed full tree left a reattested artifact: %v", err)
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
	for _, want := range []string{"channel: 'chrome'", "AIH_VISUAL_TARGETS", "for (const item of targets)", "viewport: { width: item.width, height: item.height }", "serviceWorkers: 'block'", "await context.route", "route.fetch({ maxRedirects: 0 })", "BLOCKED REDIRECT", "await context.routeWebSocket", "url.origin !== origin", "route.abort('blockedbyclient')", "ws.close()", "await ws.connectToServer()", "context.newPage", "browser.close", "network.length < 512"} {
		if !strings.Contains(visualRunner, want) {
			t.Fatalf("AIH runner omitted required policy %q", want)
		}
	}
}

func TestVisualRuntimeProbeParsesOnNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-probe.mjs")
	if err := os.WriteFile(path, []byte(visualRuntimeProbe), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("node", "--check", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runtime probe is not valid Node syntax: %v %s", err, out)
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

func TestVisualManifestRequiresEveryConfiguredTargetAndViewport(t *testing.T) {
	dir := t.TempDir()
	head, cfg := strings.Repeat("a", 40), strings.Repeat("b", 64)
	if err := writeVisualPNG(filepath.Join(dir, "desktop.png")); err != nil {
		t.Fatal(err)
	}
	if err := writeVisualPNG(filepath.Join(dir, "detail.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network.txt"), []byte("desktop GET 200 /\ndetail GET 200 /detail"), 0600); err != nil {
		t.Fatal(err)
	}
	targets := []config.VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 2, Height: 2}, {ID: "detail", Path: "/detail", Width: 2, Height: 2}}
	writeManifest := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(fmt.Sprintf(`{"head":%q,"summary":"two states","artifacts":["desktop.png","detail.png","network.txt"],"targets":[{"id":"desktop","path":"/","width":2,"height":2,"screenshot":"desktop.png"},{"id":"detail","path":"/detail","width":2,"height":2,"screenshot":"detail.png"}]}`, head))
	if _, err := loadVisualEvidenceForTargets(dir, "task-1", head, cfg, targets); err != nil {
		t.Fatalf("valid mapped targets rejected: %v", err)
	}
	writeManifest(fmt.Sprintf(`{"head":%q,"summary":"bad mapping","artifacts":["desktop.png","detail.png","network.txt"],"targets":[{"id":"desktop","path":"/","width":2,"height":2,"screenshot":"desktop.png"}]}`, head))
	if _, err := loadVisualEvidenceForTargets(dir, "task-1", head, cfg, targets); err == nil {
		t.Fatal("partial target manifest accepted")
	}
	writeManifest(fmt.Sprintf(`{"head":%q,"summary":"missing diagnostics","artifacts":["desktop.png","detail.png"],"targets":[{"id":"desktop","path":"/","width":2,"height":2,"screenshot":"desktop.png"},{"id":"detail","path":"/detail","width":2,"height":2,"screenshot":"detail.png"}]}`, head))
	if _, err := loadVisualEvidenceForTargets(dir, "task-1", head, cfg, targets); err == nil {
		t.Fatal("target manifest without shared diagnostics accepted")
	}
	writeManifest(fmt.Sprintf(`{"head":%q,"summary":"bad viewport","artifacts":["desktop.png","detail.png","network.txt"],"targets":[{"id":"desktop","path":"/","width":3,"height":2,"screenshot":"desktop.png"},{"id":"detail","path":"/detail","width":2,"height":2,"screenshot":"detail.png"}]}`, head))
	if _, err := loadVisualEvidenceForTargets(dir, "task-1", head, cfg, targets); err == nil {
		t.Fatal("mismatched screenshot dimensions accepted")
	}
}

func TestVisualInputClosureReattestsOnlyIdenticalDeclaredInputs(t *testing.T) {
	ctx := context.Background()
	repo, state := t.TempDir(), t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("config", "user.email", "test@example.invalid")
	run("config", "user.name", "Test")
	for _, name := range []string{"package-lock.json", "src/loader.ts", "assets/font.woff2", "scripts/capture.mjs"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"-one"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "first")
	first := run("rev-parse", "HEAD")
	targets := []config.VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 2, Height: 2}}
	closure := &config.VisualInputClosure{Version: 1, Runtime: "chrome-1", Targets: []config.VisualInputClosureTarget{{ID: "desktop"}}}
	effective := config.Effective{Hash: strings.Repeat("b", 64), Project: config.Project{VisualCapture: &config.VisualCapture{Server: []string{"node", "scripts/capture.mjs"}, Timeout: 10, Targets: targets, InputClosure: closure}}}
	c := &Controller{P: &Project{Dir: state}}
	receipt, err := visualInputClosure(ctx, repo, effective, "test-runtime", first)
	if err != nil {
		t.Fatal(err)
	}
	receipt.SourceHead = first
	base := filepath.Join(state, "visual-evidence", "task-closure")
	source := filepath.Join(base, first+"-"+effective.Hash[:16])
	if err = os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeVisualPNG(filepath.Join(source, "desktop.png")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "network.txt"), []byte("desktop GET 200 /"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "manifest.json"), []byte(fmt.Sprintf(`{"head":%q,"summary":"clean","artifacts":["desktop.png","network.txt"],"targets":[{"id":"desktop","path":"/","width":2,"height":2,"screenshot":"desktop.png"}]}`, first)), 0600); err != nil {
		t.Fatal(err)
	}
	evidence, err := loadVisualEvidenceForTargets(source, "task-closure", first, effective.Hash, targets)
	if err != nil {
		t.Fatal(err)
	}
	evidence.Closure, evidence.Runtime = receipt.Hash, receipt.Runtime
	if err = sealVisualEvidence(source, evidence, receipt); err != nil {
		t.Fatal(err)
	}
	sealPath := filepath.Join(source, "capture-seal.json")
	originalSeal, err := os.ReadFile(sealPath)
	if err != nil {
		t.Fatal(err)
	}
	var mismatched visualEvidenceSeal
	if err = json.Unmarshal(originalSeal, &mismatched); err != nil {
		t.Fatal(err)
	}
	mismatched.Closure.SourceHead = strings.Repeat("a", 40)
	corruptSeal, err := json.Marshal(mismatched)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(sealPath, corruptSeal, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = loadVisualSeal(source, "task-closure", first, effective.Hash, targets); err == nil {
		t.Fatal("seal accepted a closure receipt from a different source head")
	}
	if err = os.WriteFile(sealPath, originalSeal, 0600); err != nil {
		t.Fatal(err)
	}

	commit := func(path string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, path), []byte(path+time.Now().String()), 0600); err != nil {
			t.Fatal(err)
		}
		run("add", path)
		run("commit", "-m", "change "+path)
		return run("rev-parse", "HEAD")
	}
	// A routine non-closure change reattests the sealed artifact and records the
	// old capture head instead of pretending that the manifest originated here.
	run("commit", "--allow-empty", "-m", "sync")
	second := run("rev-parse", "HEAD")
	output := filepath.Join(base, second+"-"+effective.Hash[:16])
	got, ok, err := c.reattestVisualEvidence(ctx, base, output, &model.Task{ID: "task-closure", HeadSHA: second}, effective, targets, mustVisualClosure(t, ctx, repo, effective))
	if err != nil || !ok || got.Head != second || got.SourceHead != first || got.ReuseReason == "" {
		t.Fatalf("identical closure was not reattested: %#v %t %v", got, ok, err)
	}
	for _, path := range []string{"package-lock.json", "src/loader.ts", "assets/font.woff2", "scripts/capture.mjs"} {
		head := commit(path)
		candidate := filepath.Join(base, head+"-"+effective.Hash[:16])
		if _, ok, err := c.reattestVisualEvidence(ctx, base, candidate, &model.Task{ID: "task-closure", HeadSHA: head}, effective, targets, mustVisualClosure(t, ctx, repo, effective)); err != nil || ok {
			t.Fatalf("changed closure input %s was reused: ok=%t err=%v", path, ok, err)
		}
	}
	// The reviewed head is immutable even if the worktree branch advances
	// while the browser runtime probe is running.
	original, err := visualInputClosure(ctx, repo, effective, "test-runtime", first)
	if err != nil || original.Hash != receipt.Hash || original.Tree != receipt.Tree {
		t.Fatalf("closure followed moving HEAD instead of reviewed head: %#v %v", original, err)
	}
	changedRuntime := effective
	changedRuntime.Project.VisualCapture = &config.VisualCapture{Server: effective.Project.VisualCapture.Server, Timeout: 10, Targets: targets, InputClosure: &config.VisualInputClosure{Version: 1, Runtime: "chrome-2", Targets: closure.Targets}}
	if _, ok, err := c.reattestVisualEvidence(ctx, base, filepath.Join(base, "runtime"), &model.Task{ID: "task-closure", HeadSHA: run("rev-parse", "HEAD")}, changedRuntime, targets, mustVisualClosure(t, ctx, repo, changedRuntime)); err != nil || ok {
		t.Fatalf("changed runtime identity was reused: ok=%t err=%v", ok, err)
	}
	if _, ok, err := c.reattestVisualEvidence(ctx, base, filepath.Join(base, "browser-runtime"), &model.Task{ID: "task-closure", HeadSHA: run("rev-parse", "HEAD")}, effective, targets, mustVisualClosureWithRuntime(t, ctx, repo, effective, "test-runtime-2")); err != nil || ok {
		t.Fatalf("changed actual browser runtime was reused: ok=%t err=%v", ok, err)
	}
	changedConfig := effective
	changedConfig.Hash = strings.Repeat("c", 64)
	if _, ok, err := c.reattestVisualEvidence(ctx, base, filepath.Join(base, "config"), &model.Task{ID: "task-closure", HeadSHA: run("rev-parse", "HEAD")}, changedConfig, targets, mustVisualClosure(t, ctx, repo, changedConfig)); err != nil || ok {
		t.Fatalf("changed capture configuration was reused: ok=%t err=%v", ok, err)
	}
}

func mustVisualClosure(t *testing.T, ctx context.Context, dir string, effective config.Effective) *visualClosureReceipt {
	t.Helper()
	return mustVisualClosureWithRuntime(t, ctx, dir, effective, "test-runtime")
}

func mustVisualClosureWithRuntime(t *testing.T, ctx context.Context, dir string, effective config.Effective, runtime string) *visualClosureReceipt {
	t.Helper()
	head, err := (gitx.Git{Dir: dir}).SHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	closure, err := visualInputClosure(ctx, dir, effective, runtime, head)
	if err != nil {
		t.Fatal(err)
	}
	return closure
}
