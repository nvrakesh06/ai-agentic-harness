package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitLiteralEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.env")
	keys := []string{"AIH_TEST_ENV_PRECEDENCE", "AIH_TEST_ENV_LITERAL", "AIH_TEST_ENV_PATH"}
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(keys[0], "process-wins")
	data := "# comments\r\nAIH_TEST_ENV_PRECEDENCE=file\r\nAIH_TEST_ENV_LITERAL='$(touch sentinel); $HOME'\r\nAIH_TEST_ENV_PATH=\"C:\\Some Folder\\codex.exe\"\r\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvironment(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(keys[0]) != "process-wins" || os.Getenv(keys[1]) != "$(touch sentinel); $HOME" || os.Getenv(keys[2]) != `C:\Some Folder\codex.exe` {
		t.Fatal("environment was expanded or precedence changed")
	}
	for _, invalid := range []string{"export KEY=value", "KEY='unterminated", "KEY=a\nKEY=b", "KEY=bad\x00value"} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if err := LoadEnvironment(path); err == nil || strings.Contains(err.Error(), invalid) {
			t.Fatal("accepted malformed env or disclosed contents", err)
		}
	}
}

func TestRuntimeHomeSymlinkBoundary(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if OutsideSource(source, filepath.Join(source, "state")) == nil {
		t.Fatal("nested state accepted")
	}
	if err := OutsideSource(source, filepath.Join(base, "state")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "alias")
	if err := os.Symlink(source, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if OutsideSource(source, filepath.Join(link, "not-yet-created", "state")) == nil {
		t.Fatal("symlink bypass accepted")
	}
}

func TestGitHubRemoteForms(t *testing.T) {
	for _, remote := range []string{"https://github.com/owner/project.git", "git@github.com:owner/project.git", "ssh://git@github.com/owner/project.git"} {
		if repo, err := GitHubRepo(remote); err != nil || repo != "owner/project" {
			t.Fatal(remote, repo, err)
		}
	}
	for _, remote := range []string{"https://github.com/a/..", "https://github.com/a/b?token=secret", "ssh://user@github.com/a/b"} {
		if _, err := GitHubRepo(remote); err == nil {
			t.Fatal(remote)
		}
	}
	t.Setenv("AIH_RELEASE_REPO", "fork/harness")
	if ReleaseRepository(UpstreamRepository) != "fork/harness" {
		t.Fatal("release override ignored")
	}
}
