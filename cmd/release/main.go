// release builds the supported targets locally. Publishing is explicit and
// requires a clean, already tagged commit; there is no GitHub Actions dependency.
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
)

// releaseTestTimeout bounds a single package test binary. The engine package
// creates real temporary Git repositories and can take more than ten minutes
// on a loaded, serialized Windows host. Fifteen minutes leaves room for the
// complete package while retaining bounded fixture-level admission deadlines.
const releaseTestTimeout = "15m"

const releasePermitWait = 2 * time.Minute

func main() {
	if e := release(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func release() error {
	publish := flag.Bool("publish", false, "publish assets for an existing v1 tag on HEAD")
	repo := flag.String("repo", config.ReleaseRepository(""), "release repository (or AIH_RELEASE_REPO)")
	flag.Parse()
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
	interrupt, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupt, releasePermitWait)
	defer cancel()
	releaseMachine, e := releaseMachinePermit(ctx)
	if e != nil {
		return e
	}
	defer releaseMachine()
	// Serialize package workers: the suite intentionally exercises real Git and
	// process lifecycles, and concurrent package runs can make its bounded
	// Windows timings unreliable on a constrained development machine.
	for _, args := range [][]string{{"test", "-p=1", "./...", "-count=1", "-timeout", releaseTestTimeout}, {"vet", "./..."}} {
		if e := run(nil, "go", args...); e != nil {
			return e
		}
	}
	if e := os.MkdirAll("dist", 0755); e != nil {
		return e
	}
	assets := []string{}
	var sums strings.Builder
	for _, target := range [][2]string{{"windows", "amd64"}, {"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
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
		if e := run(env, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", p, "./cmd/aih"); e != nil {
			return e
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
		assets = append(assets, p)
	}
	manifest := filepath.Join("dist", "SHA256SUMS")
	if e := os.WriteFile(manifest, []byte(sums.String()), 0644); e != nil {
		return e
	}
	assets = append(assets, manifest)
	fmt.Print(sums.String())
	if *publish {
		args := append([]string{"release", "create", "v" + model.Version}, assets...)
		args = append(args, "--repo", *repo, "--verify-tag", "--title", "AIH v"+model.Version, "--notes-file", "docs/RELEASE_NOTES.md")
		return run(nil, "gh", args...)
	}
	return nil
}

// releaseMachinePermit shares the native heavy-check slots used by supervised
// application verification. A manual release gate therefore waits visibly for
// an active consumer check instead of turning resource contention into a test
// failure.
func releaseMachinePermit(ctx context.Context) (func(), error) {
	home, err := config.Home("")
	if err != nil {
		return nil, err
	}
	machine, err := config.MachineConfig(home)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "waiting up to %s for AIH machine heavy-check capacity (%d slot(s)); interrupt to cancel\n", releasePermitWait, machine.MaxHeavyChecks)
	release, err := platform.AcquireSlot(ctx, filepath.Join(home, "verification"), "heavy", machine.MaxHeavyChecks)
	if err != nil {
		return nil, fmt.Errorf("release gate resource contention: AIH machine heavy-check capacity was unavailable within %s: %w", releasePermitWait, err)
	}
	fmt.Fprintln(os.Stderr, "acquired AIH machine heavy-check capacity")
	return release, nil
}
func run(env []string, name string, args ...string) error {
	fmt.Println(name, strings.Join(args, " "))
	c := exec.Command(name, args...)
	c.Env = env
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
