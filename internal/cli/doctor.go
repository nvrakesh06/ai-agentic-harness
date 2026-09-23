package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/github"
	"github.com/nvrakesh06/ai-agentic-harness/internal/gitx"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"github.com/spf13/cobra"
)

type diagnostics struct {
	out    io.Writer
	failed bool
}

func (d *diagnostics) check(label string, err error, remedy string) bool {
	if err == nil {
		fmt.Fprintln(d.out, "[OK]", label)
		return true
	}
	d.failed = true
	// Subprocess/auth output can contain credentials or personal account data.
	// Deliberately print actionable guidance, not arbitrary tool error output.
	fmt.Fprintf(d.out, "[FAIL] %s\n       %s\n", label, remedy)
	return false
}

func (d *diagnostics) warn(message string) { fmt.Fprintln(d.out, "[WARN]", message) }

func addDoctor(root *cobra.Command, o *options) {
	var selected string
	var machineOnly, offline bool
	cmd := &cobra.Command{Use: "doctor", Short: "Check tools, authentication, storage and project configuration (no model calls)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		d := &diagnostics{out: cmd.OutOrStdout()}
		ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
		defer cancel()
		dir, home, err := o.paths()
		if err != nil {
			return err
		}
		var osErr error
		if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			osErr = errors.New("unsupported OS")
		}
		d.check("Platform "+runtime.GOOS+"/"+runtime.GOARCH, osErr, "Use Windows, Linux or macOS; Ubuntu 24.04 LTS is the primary server target.")
		fmt.Fprintf(d.out, "[INFO] AIH built with %s; no Go/Node/Python runtime is required by this binary.\n", runtime.Version())
		for _, name := range []string{"git", "gh"} {
			_, err := exec.LookPath(name)
			d.check(name+" on PATH", err, "Install "+name+" and ensure the service user has the same PATH; see docs/VM_SETUP.md.")
		}
		out, err := platform.Run(ctx, dir, nil, "", "git", "--version")
		var major, minor int
		if err == nil {
			_, err = fmt.Sscanf(out, "git version %d.%d", &major, &minor)
			if major < 2 || (major == 2 && minor < 28) {
				err = errors.New("old git")
			}
		}
		d.check("Git >= 2.28", err, "Upgrade Git (AIH requires worktrees, atomic pushes and explicit ref leases).")
		var cfg config.Effective
		project := false
		if !machineOnly {
			_, statErr := os.Stat(filepath.Join(dir, ".aih", "project.yaml"))
			if os.IsNotExist(statErr) {
				d.warn("No AIH application configuration here; machine checks only. Run aih init in your application, or aih attach for an existing project.")
			} else {
				cfg, err = config.ParseLocal(dir)
				project = d.check("Local project configuration", err, "Review .aih/project.yaml, policies.yaml and harness.lock; run aih rules doctor for canonical policy diagnostics.")
				if project {
					selected = cfg.Project.Provider
				}
			}
		}
		var p *engine.Project
		if project && !offline {
			opened, openErr := o.open(ctx, true)
			if d.check("Git repository, origin/main and canonical configuration", openErr, "Check origin, Git credentials and committed configuration on main; run aih attach on a new machine.") {
				p = opened
				defer p.DB.Close()
				if localPolicyDiffers(cfg, p.Config) {
					d.warn("Local policy differs from canonical main. Diagnostics use canonical policy; run aih rules doctor to inspect drift.")
				}
				cfg = p.Config
				selected = cfg.Project.Provider
			}
		}
		if selected != "codex" && selected != "claude-code" {
			return errors.New("provider must be codex or claude-code")
		}
		d.check(selected+" CLI capabilities", provider.New(selected).Validate(ctx), "Install/update the selected CLI or set CODEX_BINARY / CLAUDE_BINARY to an executable path; see docs/AGENTS_SETUP.md.")
		if offline {
			d.warn("Offline: authentication and remote access NOT checked. This is not a deployment readiness pass.")
		} else {
			d.check(selected+" authentication", provider.CheckAuthentication(ctx, selected), "Authenticate as the service OS user; see docs/AGENTS_SETUP.md. No model request was made.")
			_, err = platform.Run(ctx, dir, nil, "", "gh", "auth", "status", "--hostname", "github.com")
			d.check("GitHub authentication", err, "Run gh auth login --hostname github.com --git-protocol https --web, then gh auth setup-git.")
		}
		if d.check("AIH_HOME outside source", config.OutsideSource(dir, home), "Set AIH_HOME or --home to a local persistent directory outside the source checkout.") {
			d.check("Storage writable; SQLite WAL available", probeStorage(home), "Check AIH_HOME ownership, free disk space and local filesystem support (do not use NFS/cloud-synced storage).")
			fmt.Fprintln(d.out, "[INFO] Persistent machine state:", home)
		}
		if project {
			for _, warning := range cfg.Project.ModelMappingWarnings() {
				d.warn(warning)
			}
			checkExecutables(d, cfg, dir)
			if !offline {
				if p != nil {
					d.check("GitHub repository writable; issues enabled", (github.Client{Repo: p.Repo, Dir: p.Root}).Capabilities(ctx), "Grant the service account access to this unarchived GitHub repository and enable issues.")
					var integrity string
					err = p.DB.DB.QueryRow("PRAGMA quick_check").Scan(&integrity)
					if err == nil && integrity != "ok" {
						err = errors.New("integrity check failed")
					}
					d.check("Project SQLite integrity", err, "Stop AIH, preserve the entire project directory, and follow docs/RECOVERY.md.")
					if active(p) {
						fmt.Fprintln(d.out, "[INFO] Local supervisor lock is held.")
					}
					_, _, err = p.Git.Load(ctx)
					d.check("Remote aih-state readable", err, "Run aih init once for a new application, or restore the existing aih-state branch; never merge it into main.")
				}
			} else {
				_, _, err := gitx.Discover(ctx, dir)
				d.check("Application Git repository and origin", err, "Use a Git checkout with an origin remote.")
			}
		}
		fmt.Fprintln(d.out, "[INFO] No management listener or inbound port is required. Checks were not executed; provider billing/quota and remote push policy are not proven by doctor.")
		if d.failed {
			return errors.New("doctor found missing requirements; resolve the [FAIL] items")
		}
		return nil
	}}
	cmd.Flags().StringVar(&selected, "provider", "codex", "provider for machine-only checks")
	cmd.Flags().BoolVar(&machineOnly, "machine", false, "skip application checks")
	cmd.Flags().BoolVar(&offline, "offline", false, "skip authentication and remote checks; not a deployment readiness pass")
	root.AddCommand(cmd)
}

func localPolicyDiffers(local, canonical config.Effective) bool {
	for name, value := range local.Files {
		if normalizedPolicy(value) != normalizedPolicy(canonical.Files[name]) {
			return true
		}
	}
	return false
}

func normalizedPolicy(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func checkExecutables(d *diagnostics, cfg config.Effective, dir string) {
	count := 0
	for _, check := range cfg.Project.Checks {
		applicable := len(check.Platforms) == 0
		for _, osName := range check.Platforms {
			if osName == runtime.GOOS {
				applicable = true
			}
		}
		if !applicable {
			continue
		}
		count++
		name := check.Command[0]
		var err error
		if strings.ContainsAny(name, `/\`) && !filepath.IsAbs(name) {
			var info os.FileInfo
			info, err = os.Stat(filepath.Join(dir, name))
			if err == nil && (info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0111 == 0)) {
				err = errors.New("check path is not executable")
			}
		} else {
			_, err = exec.LookPath(name)
		}
		d.check("Check executable: "+check.Name, err, "Install the application's build tools/dependencies or correct this check's command on canonical main.")
	}
	var err error
	if count == 0 {
		err = errors.New("no applicable checks")
	}
	d.check("Verification configured for "+runtime.GOOS, err, "Add at least one applicable check to .aih/project.yaml and commit/push it to main.")
}

func probeStorage(home string) error {
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(home, "doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) // Only this freshly created, private diagnostic directory.
	db, err := store.Open(filepath.Join(dir, "probe.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.Set("probe", "ok"); err != nil {
		return err
	}
	var mode string
	if err = db.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return errors.New("WAL unavailable")
	}
	return nil
}
