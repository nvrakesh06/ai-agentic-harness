package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"github.com/nvrakesh06/ai-agentic-harness/internal/update"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type options struct{ home, repo string }

func (o *options) paths() (string, string, error) {
	home, e := config.Home(o.home)
	if e != nil {
		return "", "", e
	}
	root, e := filepath.Abs(o.repo)
	return root, home, e
}
func (o *options) open(ctx context.Context, refresh bool) (*engine.Project, error) {
	root, home, e := o.paths()
	if e != nil {
		return nil, e
	}
	return engine.Open(ctx, root, home, refresh)
}
func New() *cobra.Command {
	o := &options{}
	root := &cobra.Command{Use: "aih", Short: "A durable local control plane for Codex and Claude Code", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&o.home, "home", "", "machine AIH directory (or AIH_HOME)")
	root.PersistentFlags().StringVar(&o.repo, "repo", ".", "application repository")
	root.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "AIH %s · state %d · roles %d · rules %d\n", model.Version, model.StateSchema, model.RoleSchema, model.RulesVersion)
	}})
	root.AddCommand(&cobra.Command{Use: "install", Short: "Initialize this machine's AIH workspace", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, h, e := o.paths()
		if e != nil {
			return e
		}
		m, e := config.Install(h)
		if e == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Machine %s registered at %s\n", m.ID, h)
		}
		return e
	}})
	var selected string
	initCmd := &cobra.Command{Use: "init", Short: "Enable a repository and initialize remote AIH state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		r, h, e := o.paths()
		if e != nil {
			return e
		}
		if e = engine.Init(cmd.Context(), r, h, selected); e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), "AIH initialized. Review .aih/project.yaml and AGENTS.md, then commit and push the configuration to main. Run aih run \"your objective\" afterward.")
		return nil
	}}
	initCmd.Flags().StringVar(&selected, "provider", "codex", "codex or claude-code")
	root.AddCommand(initCmd)
	for _, name := range []string{"attach", "sync"} {
		name := name
		root.AddCommand(&cobra.Command{Use: name, Short: "Rebuild local state from Git and GitHub", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			p, e := o.open(cmd.Context(), true)
			if e != nil {
				return e
			}
			defer p.DB.Close()
			if active(p) {
				return errors.New("local supervisor is active; stop it before rebuilding its cache")
			}
			if e = p.Attach(cmd.Context()); e != nil {
				return e
			}
			checkUpdate(cmd, p)
			fmt.Fprintln(cmd.OutOrStdout(), "Recovered durable state and task worktrees. Use aih resume to execute work.")
			return nil
		}})
	}
	for _, name := range []string{"start", "resume", "takeover"} {
		var foreground bool
		cmd := &cobra.Command{Use: name, Short: "Start the controller (live remote leases are never overridden)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			p, e := o.open(cmd.Context(), true)
			if e != nil {
				return e
			}
			defer p.DB.Close()
			checkUpdate(cmd, p)
			if foreground {
				return engine.New(p).Serve(cmd.Context())
			}
			return background(cmd, p)
		}}
		cmd.Flags().BoolVar(&foreground, "foreground", false, "run in this terminal")
		root.AddCommand(cmd)
	}
	var file string
	run := &cobra.Command{Use: "run [requirement]", Short: "Submit an objective and ensure the supervisor is running", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		requirement := ""
		if file != "" {
			if len(args) > 0 {
				return errors.New("use either a requirement or --file")
			}
			b, e := os.ReadFile(file)
			if e != nil {
				return e
			}
			requirement = string(b)
		} else if len(args) > 0 {
			requirement = args[0]
		}
		if strings.TrimSpace(requirement) == "" {
			return errors.New("requirement is empty")
		}
		p, e := o.open(cmd.Context(), true)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		id := model.ID()
		if e = p.DB.Submit(store.Command{ID: id, Kind: "run", Payload: requirement}); e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Queued objective %s locally; remote acceptance is shown in status.\n", id)
		return background(cmd, p)
	}}
	run.Flags().StringVar(&file, "file", "", "read objective from a UTF-8 file")
	root.AddCommand(run)
	root.AddCommand(&cobra.Command{Use: "answer <task-or-objective> <answer>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		p, e := o.open(cmd.Context(), false)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		id := model.ID()
		if e = p.DB.Submit(store.Command{ID: id, Kind: "answer", Target: args[0], Payload: args[1]}); e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Queued answer %s.\n", id)
		return background(cmd, p)
	}})
	for _, name := range []string{"stop", "handoff"} {
		name := name
		root.AddCommand(&cobra.Command{Use: name, Short: "Stop scheduling, checkpoint workers, and release the lease", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			p, e := o.open(cmd.Context(), false)
			if e != nil {
				return e
			}
			defer p.DB.Close()
			if !active(p) {
				return errors.New("no local supervisor is running")
			}
			if e = p.DB.Submit(store.Command{ID: model.ID(), Kind: name}); e != nil {
				return e
			}
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			deadline := time.NewTimer(3 * time.Minute)
			defer deadline.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-deadline.C:
					return errors.New("shutdown still pending; inspect aih logs before takeover")
				case <-ticker.C:
					if !active(p) {
						if msg := p.DB.Get("last_error"); msg != "" {
							return errors.New(msg)
						}
						fmt.Fprintln(cmd.OutOrStdout(), "Supervisor stopped; checkpoints persisted and controller lease released.")
						return nil
					}
				}
			}
		}})
	}
	for _, name := range []string{"status", "blockers", "watch"} {
		name := name
		var asJSON bool
		cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			p, e := o.open(cmd.Context(), false)
			if e != nil {
				return e
			}
			defer p.DB.Close()
			for {
				if e = showStatus(cmd, p, name == "blockers", asJSON); e != nil {
					return e
				}
				if name != "watch" {
					return nil
				}
				timer := time.NewTimer(2 * time.Second)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		}}
		cmd.Flags().BoolVar(&asJSON, "json", false, "emit portable state as JSON")
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{Use: "logs", Short: "Show recent structured supervisor events", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		p, e := o.open(cmd.Context(), false)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		rows, e := p.DB.DB.Query("SELECT at,task,run,role,provider,kind,message FROM events ORDER BY id DESC LIMIT 100")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var at, task, run, role, prov, kind, msg string
			if e = rows.Scan(&at, &task, &run, &role, &prov, &kind, &msg); e != nil {
				return e
			}
			b, _ := json.Marshal(map[string]string{"timestamp": at, "project": p.Config.Project.ID, "task": task, "run": run, "role": role, "provider": prov, "event": kind, "message": msg})
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Worker diagnostics:", filepath.Join(p.Dir, "sessions"))
		fmt.Fprintln(cmd.OutOrStdout(), "Supervisor output:", filepath.Join(p.Dir, "logs", "supervisor.log"))
		return rows.Err()
	}})
	addInspection(root, o)
	addUpdate(root, o)
	root.AddCommand(&cobra.Command{Use: "demo", Short: "Run a deterministic workflow and destructive local-cache recovery in a disposable fixture", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		exe, e := os.Executable()
		if e != nil {
			return e
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		path, e := demo.Run(ctx, cmd.OutOrStdout(), []string{exe, "_demo-check"})
		fmt.Fprintln(cmd.OutOrStdout(), "Demo artifacts:", path)
		return e
	}})
	root.AddCommand(&cobra.Command{Use: "_demo-check", Hidden: true, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, e := os.Getwd()
		if e != nil {
			return e
		}
		return demo.Check(dir)
	}})
	root.AddCommand(&cobra.Command{Use: "improve <observation>", Short: "Record a reusable harness improvement candidate", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, e := o.open(cmd.Context(), false)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		if e = p.DB.Submit(store.Command{ID: model.ID(), Kind: "improvement", Payload: args[0]}); e != nil {
			return e
		}
		return background(cmd, p)
	}})
	return root
}
func active(p *engine.Project) bool {
	l, e := platform.Acquire(filepath.Join(p.Dir, "supervisor.lock"))
	if e != nil {
		return true
	}
	_ = l.Close()
	return false
}
func background(cmd *cobra.Command, p *engine.Project) error {
	if active(p) {
		fmt.Fprintln(cmd.OutOrStdout(), "Local supervisor is running.")
		return nil
	}
	if e := p.Provider.Validate(cmd.Context()); e != nil {
		return e
	}
	s, _, e := p.Git.Load(cmd.Context())
	if e != nil {
		return e
	}
	if s.Controller.Owner != "" && s.Controller.Expires.Add(5*time.Second).After(time.Now()) {
		return fmt.Errorf("controller lease held by %s until %s", s.Controller.Machine, s.Controller.Expires)
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	logs := filepath.Join(p.Dir, "logs")
	if e = os.MkdirAll(logs, 0700); e != nil {
		return e
	}
	_ = p.DB.Set("last_error", "")
	if e = platform.Background(exe, []string{"start", "--foreground", "--repo", p.Root, "--home", p.Home}, p.Root, filepath.Join(logs, "supervisor.log")); e != nil {
		return e
	}
	for i := 0; i < 30; i++ {
		if active(p) && p.DB.Get("pid") != "" {
			fmt.Fprintln(cmd.OutOrStdout(), "Supervisor started. Use aih status or aih watch.")
			return nil
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("supervisor did not acknowledge startup; inspect %s", filepath.Join(logs, "supervisor.log"))
}
func checkUpdate(cmd *cobra.Command, p *engine.Project) {
	if p.Config.Lock.AutoUpdate == "off" {
		return
	}
	latest, e := update.Latest(cmd.Context(), p.Config.Project.ReleaseRepo)
	if e != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Update check unavailable; continuing with", model.Version)
		return
	}
	if config.CompareVersion(latest, model.Version) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "AIH %s is available; run aih self-update for a verified staged update.\n", latest)
	}
}
func showStatus(cmd *cobra.Command, p *engine.Project, blockers, asJSON bool) error {
	s, h, e := p.DB.Load()
	if e != nil {
		return fmt.Errorf("no cached state; run aih attach: %w", e)
	}
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Project %s · durable revision %d (%s)\n", s.Project, s.Revision, shortSHA(h))
	fmt.Fprintf(cmd.OutOrStdout(), "Controller: %s · lease expires %s\n", s.Controller.Machine, s.Controller.Expires.Format(time.RFC3339))
	for _, t := range model.Ordered(s) {
		if blockers && t.State != model.Blocked {
			continue
		}
		status := string(t.State)
		if t.State == model.Ready {
			for _, d := range t.Dependencies {
				if s.Tasks[d] != nil && s.Tasks[d].State != model.Done {
					status = "WAITING_DEPENDENCIES"
				}
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "#%-5d %-22s %s [%s]\n", t.Issue, status, t.Title, t.ID)
		if t.Blocker != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n  Reason: %s\n", t.Blocker.Question, t.Blocker.Reason)
		}
	}
	for _, ob := range s.Objectives {
		if ob.Blocker != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "Objective %s needs input: %s\n", ob.ID, ob.Blocker)
		}
	}
	if s.IntegrationBlocked != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Further integration paused by post-merge failure:", s.IntegrationBlocked)
	}
	commands, e := p.DB.Pending()
	if e == nil && len(commands) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "%d local request(s) awaiting remote acceptance\n", len(commands))
	}
	rows, re := p.DB.DB.Query("SELECT id,error FROM commands WHERE status='failed' ORDER BY rowid DESC LIMIT 5")
	if re == nil {
		defer rows.Close()
		for rows.Next() {
			var id, msg string
			if rows.Scan(&id, &msg) == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Rejected local request %s: %s\n", id, msg)
			}
		}
	}
	return e
}
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
func addInspection(root *cobra.Command, o *options) {
	roleCmd := &cobra.Command{Use: "roles", RunE: func(cmd *cobra.Command, _ []string) error {
		p, e := o.open(cmd.Context(), false)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		all, e := roles.Load(p.Config.Files)
		if e != nil {
			return e
		}
		names := []string{}
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := all[n]
			fmt.Fprintf(cmd.OutOrStdout(), "%-28s %-18s %s\n", n, r.Stage, r.Capability)
		}
		return nil
	}}
	roleCmd.AddCommand(&cobra.Command{Use: "explain <role>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, e := o.open(cmd.Context(), false)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		all, e := roles.Load(p.Config.Files)
		if e != nil {
			return e
		}
		r, ok := all[args[0]]
		if !ok {
			return errors.New("unknown role")
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}})
	root.AddCommand(roleCmd)
	roleCmd.AddCommand(&cobra.Command{Use: "assign <task-id> <role>", Args: cobra.ExactArgs(2), Short: "Add a specialist to an idle task", RunE: func(cmd *cobra.Command, args []string) error {
		p, e := o.open(cmd.Context(), true)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		if e = p.DB.Submit(store.Command{ID: model.ID(), Kind: "assign-role", Target: args[0], Payload: args[1]}); e != nil {
			return e
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Role assignment queued; inspect status for acceptance.")
		return background(cmd, p)
	}})
	var ruleRole, ruleTask string
	var coreOnly bool
	rulesCmd := &cobra.Command{Use: "rules", RunE: func(cmd *cobra.Command, _ []string) error {
		if coreOnly {
			fmt.Fprintln(cmd.OutOrStdout(), roles.Core)
			return nil
		}
		p, e := o.open(cmd.Context(), true)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		all, e := roles.Load(p.Config.Files)
		if e != nil {
			return e
		}
		r, ok := all[ruleRole]
		if !ok {
			return errors.New("unknown role")
		}
		var task *model.Task
		if ruleTask != "" {
			s, _, err := p.DB.Load()
			if err != nil {
				return err
			}
			task = s.Tasks[ruleTask]
			if task == nil {
				return errors.New("unknown task ID")
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Engineering rules %d (%s), canonical base %s\n\n%s\n", model.RulesVersion, roles.Hash(), p.Config.BaseSHA, roles.Compile(p.Config, r, runtime.GOOS, task, "", "", ""))
		return nil
	}}
	rulesCmd.Flags().StringVar(&ruleRole, "role", "implementer", "role context to inspect")
	rulesCmd.Flags().StringVar(&ruleTask, "task", "", "include an assigned task and scoped instructions")
	rulesCmd.Flags().BoolVar(&coreOnly, "core", false, "show only the embedded engineering constitution")
	rulesCmd.AddCommand(&cobra.Command{Use: "doctor", RunE: func(cmd *cobra.Command, _ []string) error {
		p, e := o.open(cmd.Context(), true)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		for n, canonical := range p.Config.Files {
			b, re := os.ReadFile(filepath.Join(p.Root, filepath.FromSlash(n)))
			if re != nil || strings.TrimSpace(string(b)) != strings.TrimSpace(canonical) {
				fmt.Fprintln(cmd.OutOrStdout(), "Local differs from canonical main:", n)
			}
		}
		_, e = roles.Load(p.Config.Files)
		if e == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "Canonical rules and role definitions are valid.")
		}
		return e
	}})
	root.AddCommand(rulesCmd)
	var doctorProvider string
	doctorCmd := &cobra.Command{Use: "doctor", RunE: func(cmd *cobra.Command, _ []string) error {
		failed := false
		for _, name := range []string{"git", "gh"} {
			if _, e := exec.LookPath(name); e != nil {
				fmt.Fprintln(cmd.OutOrStdout(), name, ": missing")
				failed = true
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), name, ": installed")
			}
		}
		rootDir, _, pe := o.paths()
		if pe != nil {
			return pe
		}
		if _, se := os.Stat(filepath.Join(rootDir, ".aih", "project.yaml")); os.IsNotExist(se) {
			if doctorProvider != "codex" && doctorProvider != "claude-code" {
				return errors.New("provider must be codex or claude-code")
			}
			if e := provider.New(doctorProvider).Validate(cmd.Context()); e != nil {
				fmt.Fprintln(cmd.OutOrStdout(), e)
				failed = true
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), doctorProvider, ": CLI capabilities available")
			}
			if _, e := platform.Run(cmd.Context(), rootDir, nil, "", "gh", "auth", "status"); e != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "GitHub authentication unavailable")
				failed = true
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "GitHub authentication available")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "No AIH project configuration here; machine checks only.")
			if failed {
				return errors.New("doctor found missing requirements")
			}
			return nil
		}
		p, e := o.open(cmd.Context(), true)
		if e != nil {
			return e
		}
		defer p.DB.Close()
		if e = p.Provider.Validate(cmd.Context()); e != nil {
			fmt.Fprintln(cmd.OutOrStdout(), e)
			failed = true
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), p.Provider.Name(), ": CLI capabilities available")
		}
		_, e = platform.Run(cmd.Context(), p.Root, nil, "", "gh", "auth", "status")
		if e != nil {
			fmt.Fprintln(cmd.OutOrStdout(), "GitHub authentication unavailable")
			failed = true
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "GitHub authentication available")
		}
		var integrity string
		if e = p.DB.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); e != nil || integrity != "ok" {
			failed = true
		}
		fmt.Fprintln(cmd.OutOrStdout(), "SQLite:", integrity)
		if len(p.Config.Project.Checks) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No verification commands configured")
			failed = true
		}
		if failed {
			return errors.New("doctor found missing requirements")
		}
		return nil
	}}
	doctorCmd.Flags().StringVar(&doctorProvider, "provider", "codex", "provider to check outside an enabled project")
	root.AddCommand(doctorCmd)
}
func addUpdate(root *cobra.Command, o *options) {
	root.AddCommand(&cobra.Command{Use: "self-update", Short: "Download and verify the latest compatible stable release", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, home, e := o.paths()
		if e != nil {
			return e
		}
		repo := "nvrakesh06/ai-agentic-harness"
		latest, e := update.Latest(cmd.Context(), repo)
		if e != nil {
			return e
		}
		if config.CompareVersion(latest, model.Version) <= 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "Already on the latest available compatible release.")
			return nil
		}
		asset, e := update.Stage(cmd.Context(), home, repo, latest)
		if e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Verified update staged at %s\nStop all AIH supervisors, then use scripts/install.ps1 -Source or scripts/install.sh --source with this path. Running binaries were not replaced.\n", asset)
		return nil
	}})
}
