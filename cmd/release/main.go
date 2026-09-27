// release builds the supported targets locally. Publishing is explicit and
// requires a clean, already tagged commit; there is no GitHub Actions dependency.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

// releaseTestTimeout bounds each test binary. Large integration packages run
// their complete discovered inventory in serial groups, retaining this bound.
const releaseTestTimeout = "15m"

const releasePermitWait = 2 * time.Minute
const maxReleasePermitWait = 30 * time.Minute

var releaseTestCommand = func() (string, []string) {
	return "go", []string{"test", "-json", "-p=1", "./...", "-count=1", "-failfast", "-timeout", releaseTestTimeout}
}

var acquireReleaseMachinePermitFn = acquireReleaseMachinePermit
var runCompleteReleaseTestsFn = runCompleteReleaseTests

func main() {
	if e := release(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func release() error {
	publish := flag.Bool("publish", false, "publish assets for an existing v1 tag on HEAD")
	repo := flag.String("repo", config.ReleaseRepository(""), "release repository (or AIH_RELEASE_REPO)")
	capacityWait := flag.String("capacity-wait", releasePermitWait.String(), "maximum time to wait for shared heavy-check capacity (positive duration, at most 30m)")
	flag.Parse()
	wait, e := parseReleasePermitWait(*capacityWait)
	if e != nil {
		return fmt.Errorf("release --capacity-wait: %w", e)
	}
	if !config.ValidRepository(*repo) {
		return fmt.Errorf("release repository must be owner/repository")
	}
	if _, e := os.Stat("go.mod"); e != nil {
		return fmt.Errorf("run from repository root: %w", e)
	}
	if *publish {
		status, e := exec.Command("git", "status", "--porcelain").Output()
		if e != nil {
			return e
		}
		if len(status) > 0 {
			return fmt.Errorf("release requires a clean worktree")
		}
		tag, e := exec.Command("git", "describe", "--tags", "--exact-match", "HEAD").Output()
		if e != nil || strings.TrimSpace(string(tag)) != "v"+model.Version {
			return fmt.Errorf("HEAD must be tagged v%s", model.Version)
		}
	}
	ctx, cancel, releasePermit, e := runReleaseTestsWithYieldRetry(wait)
	if e != nil {
		return e
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
		if releasePermit != nil {
			releasePermit()
		}
	}()
	if e := runReleaseBoundedUnit(ctx, nil, "go", "vet", "./..."); e != nil {
		return e
	}
	if ctx, cancel, releasePermit, e = releaseTailHandoff(ctx, cancel, releasePermit, wait); e != nil {
		return e
	}
	if e := os.MkdirAll("dist", 0755); e != nil {
		return e
	}
	assets := []string{}
	var sums strings.Builder
	targets := [][2]string{{"windows", "amd64"}, {"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}}
	for index, target := range targets {
		name := "aih_" + target[0] + "_" + target[1]
		if target[0] == "windows" {
			name += ".exe"
		}
		p := filepath.Join("dist", name)
		env := []string{}
		for _, v := range os.Environ() {
			k := strings.SplitN(v, "=", 2)[0]
			if k != "GOOS" && k != "GOARCH" && k != "CGO_ENABLED" {
				env = append(env, v)
			}
		}
		env = append(env, "GOOS="+target[0], "GOARCH="+target[1], "CGO_ENABLED=0")
		if e := runReleaseBoundedUnit(ctx, env, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", p, "./cmd/aih"); e != nil {
			return e
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
		assets = append(assets, p)
		if index+1 < len(targets) {
			if ctx, cancel, releasePermit, e = releaseTailHandoff(ctx, cancel, releasePermit, wait); e != nil {
				return e
			}
		}
	}
	manifest := filepath.Join("dist", "SHA256SUMS")
	if e := os.WriteFile(manifest, []byte(sums.String()), 0644); e != nil {
		return e
	}
	assets = append(assets, manifest)
	fmt.Print(sums.String())
	if *publish {
		// The verification gate is complete. Publishing has its own external
		// atomicity rules and must not be interrupted by a later local waiter.
		// Publishing does not need the heavy-check slot.
		cancel()
		releasePermit()
		args := append([]string{"release", "create", "v" + model.Version}, assets...)
		args = append(args, "--repo", *repo, "--verify-tag", "--title", "AIH v"+model.Version, "--notes-file", "docs/RELEASE_NOTES.md")
		return run(context.Background(), nil, "gh", args...)
	}
	return nil
}

func runReleaseBoundedUnit(ctx context.Context, env []string, name string, args ...string) error {
	if _, err := releaseInvocationIdentity(ctx); err != nil {
		return err
	}
	unit, cancel := context.WithTimeout(ctx, releaseUnitWatchdog)
	defer cancel()
	if err := run(unit, env, name, args...); err != nil {
		return err
	}
	_, err := releaseInvocationIdentity(unit)
	return err
}

// releaseTailHandoff advances a finite vet/build cursor only after the
// completed command's child has closed. It does not reset the test-group
// no-progress accounting and preserves ordinary priority-aware admission.
func releaseTailHandoff(ctx context.Context, cancel context.CancelFunc, releasePermit func(), wait time.Duration) (context.Context, context.CancelFunc, func(), error) {
	b, ok := ctx.Value(releaseBoundaryKey{}).(releaseBoundary)
	if !ok {
		return nil, nil, nil, errors.New("release tail has no resource identity")
	}
	demand, err := releasePriorityDemand(b.dir, b.machine)
	if err != nil || !demand {
		return ctx, cancel, releasePermit, err
	}
	cancel()
	releasePermit()
	releaseMachine, machine, verificationDir, err := acquireReleaseMachinePermitFn(wait)
	if err != nil {
		return nil, nil, nil, err
	}
	nextCancel := context.CancelFunc(func() {})
	next, closeNext := context.WithCancel(context.Background())
	nextCancel = closeNext
	return withReleaseBoundary(next, verificationDir, machine, b.progress), nextCancel, sync.OnceFunc(releaseMachine), nil
}

// runReleaseTestsWithYieldRetry releases the shared slot only after the
// current managed process has observed cancellation. Completed exact-identity
// groups are recorded by runCompleteReleaseTests; a yielded current group has
// no receipt and is rerun after reacquiring normal shared capacity.
func runReleaseTestsWithYieldRetry(wait time.Duration) (context.Context, context.CancelFunc, func(), error) {
	progress := newReleaseInvocationProgress()
	for {
		releaseMachine, machine, verificationDir, err := acquireReleaseMachinePermitFn(wait)
		if err != nil {
			return nil, nil, nil, err
		}
		releasePermit := sync.OnceFunc(releaseMachine)
		// Priority demand is observed after a truthful bounded unit, never by
		// canceling its owned process. Explicit cancellation still flows through
		// this context normally.
		ctx, cancel := context.WithCancel(context.Background())
		ctx = withReleaseBoundary(ctx, verificationDir, machine, progress)
		beforeHandoffs := progress.handoffs
		err = runCompleteReleaseTestsFn(ctx)
		if !errors.Is(err, errReleaseYielded) {
			if err != nil {
				cancel()
				releasePermit()
				return nil, nil, nil, err
			}
			return ctx, cancel, releasePermit, nil
		}
		cancel()
		releasePermit()
		// Test seams may return the sentinel directly. Treat that as no-progress;
		// production boundary handoff has already recorded its validated outcome.
		if progress.handoffs == beforeHandoffs {
			if progressErr := progress.handoff(false); progressErr != nil {
				return nil, nil, nil, progressErr
			}
		}
		fmt.Fprintln(os.Stderr, "release gate yielded at a completed unit boundary; reacquiring priority-aware capacity")
	}
}

// runReleaseTests stops the serial suite when go test reports that a package
// failed. The JSON stream distinguishes package results from arbitrary test
// output, so a log line containing the word "fail" cannot cancel the gate.
func runReleaseTests(ctx context.Context) error {
	name, args := releaseTestCommand()
	return runReleaseTestCommand(ctx, name, args, nil)
}

func runReleaseTestCommand(ctx context.Context, name string, args []string, expected []string) error {
	return runReleaseTestGroupCommand(ctx, name, args, expected, nil)
}

func runReleaseTestGroupCommand(ctx context.Context, name string, args []string, expectedTests, expectedPackages []string) error {
	fmt.Println(name, strings.Join(args, " "))
	process, err := platform.StartManaged(ctx, "", nil, name, args...)
	if err != nil {
		if errors.Is(context.Cause(ctx), errReleaseYielded) {
			return errReleaseYielded
		}
		return err
	}
	defer process.Close()
	go func() { <-ctx.Done(); process.Close() }()

	var failedPackage string
	var failedTest string
	completedTests := map[string]string{}
	completedPackages := map[string]string{}
	scanner := bufio.NewScanner(process.Stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stdout, line)
		var event struct {
			Action  string
			Package string
			Test    string
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Action == "pass" || event.Action == "skip" {
			if event.Test != "" {
				completedTests[event.Test] = event.Action
			} else if event.Package != "" {
				completedPackages[event.Package] = event.Action
			}
		}
		if event.Action == "fail" && event.Package != "" && event.Test == "" {
			failedPackage = event.Package
			process.Close()
			break
		}
		if event.Action == "fail" && event.Package != "" && event.Test != "" && failedTest == "" {
			failedTest = event.Package + "/" + event.Test
		}
	}
	scanErr := scanner.Err()
	waitErr := process.Wait()
	if stderr := process.Stderr(); stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
	if failedPackage != "" {
		return fmt.Errorf("go test reported failure for %s; cancelled remaining package tests: %w", failedPackage, waitErr)
	}
	if failedTest != "" {
		return fmt.Errorf("go test reported failure for %s before its package terminal event: %w", failedTest, waitErr)
	}
	if errors.Is(context.Cause(ctx), errReleaseYielded) {
		return errReleaseYielded
	}
	if scanErr != nil {
		return fmt.Errorf("read go test JSON output: %w", scanErr)
	}
	if waitErr != nil {
		return waitErr
	}
	for _, test := range expectedTests {
		if !releaseTerminalAccepted(test, completedTests[test]) {
			if completedTests[test] == "skip" {
				return fmt.Errorf("release browser test %s was skipped while AIH_REAL_PLAYWRIGHT=1", test)
			}
			return fmt.Errorf("release test inventory incomplete: %s has no terminal pass/skip event", test)
		}
	}
	for _, pkg := range expectedPackages {
		if action := completedPackages[pkg]; action != "pass" && action != "skip" {
			return fmt.Errorf("release package inventory incomplete: %s has no terminal pass/skip event", pkg)
		}
	}
	return nil
}

// acquireReleaseMachinePermit owns its interrupt handler only while waiting for
// capacity. Once the release starts child commands retain normal Ctrl+C
// behavior instead of having this process consume the signal while holding the
// machine slot.
func parseReleasePermitWait(value string) (time.Duration, error) {
	wait, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a duration: %w", err)
	}
	if wait <= 0 || wait > maxReleasePermitWait {
		return 0, fmt.Errorf("must be greater than zero and no more than %s", maxReleasePermitWait)
	}
	return wait, nil
}

func acquireReleaseMachinePermit(wait time.Duration) (func(), config.Machine, string, error) {
	if wait <= 0 || wait > maxReleasePermitWait {
		return nil, config.Machine{}, "", fmt.Errorf("release capacity wait must be greater than zero and no more than %s", maxReleasePermitWait)
	}
	interrupt, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupt, wait)
	defer cancel()
	return releaseMachinePermitWithWait(ctx, wait)
}

// releaseMachinePermit shares the native heavy-check slots used by supervised
// application verification. A manual release gate therefore waits visibly for
// an active consumer check instead of turning resource contention into a test
// failure.
func releaseMachinePermit(ctx context.Context) (func(), config.Machine, string, error) {
	return releaseMachinePermitWithWait(ctx, releasePermitWait)
}

func releaseMachinePermitWithWait(ctx context.Context, wait time.Duration) (func(), config.Machine, string, error) {
	home, err := config.Home("")
	if err != nil {
		return nil, config.Machine{}, "", err
	}
	machine, err := config.MachineConfig(home)
	if err != nil {
		return nil, config.Machine{}, "", err
	}
	fmt.Fprintf(os.Stderr, "waiting up to %s for AIH machine heavy-check capacity (%d slot(s)); interrupt to cancel\n", wait, machine.MaxHeavyChecks)
	dir := filepath.Join(home, "verification")
	for {
		if err := ctx.Err(); err != nil {
			return nil, config.Machine{}, "", fmt.Errorf("release gate resource contention: AIH machine heavy-check capacity was unavailable within %s: %w", wait, err)
		}
		waiting, err := platform.HasPriorityWaiter(dir, "heavy")
		if err != nil {
			return nil, config.Machine{}, "", err
		}
		if !waiting {
			release, acquired, err := platform.TryAcquireSlot(dir, "heavy", machine.MaxHeavyChecks)
			if err != nil {
				return nil, config.Machine{}, "", err
			}
			if acquired {
				// A supervisor may have registered after the first check.
				// Return the slot if it has no spare capacity for that waiter.
				waiting, err = platform.HasPriorityWaiter(dir, "heavy")
				if err != nil {
					release()
					return nil, config.Machine{}, "", err
				}
				if waiting {
					spare, spareErr := platform.HasAvailableSlot(dir, "heavy", machine.MaxHeavyChecks)
					if spareErr != nil {
						release()
						return nil, config.Machine{}, "", spareErr
					}
					if !spare {
						release()
					} else {
						fmt.Fprintln(os.Stderr, "acquired AIH machine heavy-check capacity")
						return release, machine, dir, nil
					}
				} else {
					fmt.Fprintln(os.Stderr, "acquired AIH machine heavy-check capacity")
					return release, machine, dir, nil
				}
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, config.Machine{}, "", fmt.Errorf("release gate resource contention: AIH machine heavy-check capacity was unavailable within %s: %w", wait, ctx.Err())
		case <-timer.C:
		}
	}
}

var errReleaseYielded = errors.New("release gate yielded to queued supervisor native check")

// releaseYieldCancellation preserves the yield sentinel only for cancellation
// caused by a priority yield. Concrete test/package failures remain dominant.
func releaseYieldCancellation(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errReleaseYielded) && errors.Is(err, context.Canceled) {
		return errReleaseYielded
	}
	return nil
}

func releaseYieldContext(dir string, machine config.Machine) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				waiting, err := platform.HasPriorityWaiter(dir, "heavy")
				if err != nil || !waiting {
					continue
				}
				spare, err := platform.HasAvailableSlot(dir, "heavy", machine.MaxHeavyChecks)
				if err == nil && !spare {
					cancel(errReleaseYielded)
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil) }
}

func run(ctx context.Context, env []string, name string, args ...string) error {
	fmt.Println(name, strings.Join(args, " "))
	process, err := platform.StartManaged(ctx, "", env, name, args...)
	if err != nil {
		if errors.Is(context.Cause(ctx), errReleaseYielded) {
			return errReleaseYielded
		}
		return err
	}
	defer process.Close()
	go func() { <-ctx.Done(); process.Close() }()
	_, readErr := io.Copy(os.Stdout, process.Stdout)
	waitErr := process.Wait()
	if stderr := process.Stderr(); stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
	if errors.Is(context.Cause(ctx), errReleaseYielded) {
		return errReleaseYielded
	}
	if readErr != nil {
		return readErr
	}
	return waitErr
}
