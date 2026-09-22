// release builds the supported targets locally. Publishing is explicit and
// requires a clean, already tagged commit; there is no GitHub Actions dependency.
package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func main() {
	if e := release(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func release() error {
	publish := flag.Bool("publish", false, "publish assets for an existing v1 tag on HEAD")
	repo := flag.String("repo", "nvrakesh06/ai-agentic-harness", "release repository")
	flag.Parse()
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
	for _, args := range [][]string{{"test", "./...", "-timeout", "6m"}, {"vet", "./..."}} {
		if e := run(nil, "go", args...); e != nil {
			return e
		}
	}
	if e := os.MkdirAll("dist", 0755); e != nil {
		return e
	}
	assets := []string{}
	var sums strings.Builder
	for _, target := range [][2]string{{"windows", "amd64"}, {"darwin", "arm64"}, {"darwin", "amd64"}} {
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
func run(env []string, name string, args ...string) error {
	fmt.Println(name, strings.Join(args, " "))
	c := exec.Command(name, args...)
	c.Env = env
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
