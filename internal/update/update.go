package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func Compatible(version string) bool {
	return regexp.MustCompile(`^v?1\.[0-9]+\.[0-9]+$`).MatchString(version)
}
func Latest(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, e := platform.Run(ctx, "", nil, "", "gh", "api", "repos/"+repo+"/releases?per_page=100")
	if e != nil {
		return "", e
	}
	var releases []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if e = json.Unmarshal([]byte(out), &releases); e != nil {
		return "", e
	}
	best := model.Version
	for _, r := range releases {
		if !r.Draft && !r.Prerelease && Compatible(r.Tag) && config.CompareVersion(r.Tag, best) > 0 {
			best = strings.TrimPrefix(r.Tag, "v")
		}
	}
	return best, nil
}
func Asset() string {
	n := "aih_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		n += ".exe"
	}
	return n
}
func Stage(ctx context.Context, home, repo, version string) (string, error) {
	if !Compatible(version) {
		return "", errors.New("automatic update accepts stable V1 releases only; major versions require migration")
	}
	version = strings.TrimPrefix(version, "v")
	dir := filepath.Join(home, "cache", "releases", version, model.ID())
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	_, e := platform.Run(ctx, "", nil, "", "gh", "release", "download", "v"+version, "--repo", repo, "--pattern", Asset(), "--pattern", "SHA256SUMS", "--dir", dir)
	if e != nil {
		return "", e
	}
	checks, e := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if e != nil {
		return "", e
	}
	binary, e := os.ReadFile(filepath.Join(dir, Asset()))
	if e != nil {
		return "", e
	}
	if e = Verify(binary, string(checks), Asset()); e != nil {
		return "", e
	}
	return filepath.Join(dir, Asset()), nil
}
func Verify(data []byte, manifest, asset string) error {
	sum := sha256.Sum256(data)
	for _, line := range strings.Split(manifest, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && strings.TrimPrefix(parts[1], "*") == asset {
			if strings.EqualFold(parts[0], hex.EncodeToString(sum[:])) {
				return nil
			}
			return errors.New("release checksum mismatch")
		}
	}
	return fmt.Errorf("release manifest has no checksum for %s", asset)
}
