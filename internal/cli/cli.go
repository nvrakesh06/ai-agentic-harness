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
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/store"
	"github.com/nvrakesh06/ai-agentic-harness/internal/update"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type options struct{ home, repo, envFile string }

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
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if o.envFile != "" {
			return config.LoadEnvironment(o.envFile)
		}
		return nil
	}
	root.PersistentFlags().StringVar(&o.home, "home", "", "machine AIH directory (or AIH_HOME)")
	root.PersistentFlags().StringVar(&o.repo, "repo", ".", "application repository")
	root.PersistentFlags().StringVar(&o.envFile, "env-file", "", "explicit literal KEY=VALUE file; existing environment takes precedence")
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
		if cfg, parseErr := config.ParseLocal(r); parseErr == nil {
			for _, warning := range modelMappingWarnings(cfg) {
				fmt.Fprintln(cmd.OutOrStdout(), "[WARN]", warning)
			}
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
		fmt.Fprintln(cmd.OutOrStdout(), "Detached supervisor output (foreground/systemd uses terminal/journal):", filepath.Join(p.Dir, "logs", "supervisor.log"))
		return rows.Err()
	}})
	addInspection(root, o)
	addDoctor(root, o)
	addService(root, o)
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

const startupAckTimeout = 45 * time.Second

var errStartupAckTimeout = errors.New("supervisor startup acknowledgement delayed")

func waitForSupervisorStart(ctx context.Context, timeout, interval time.Duration, ready func() bool, failure func() string) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ready() {
			return nil
		}
		if message := failure(); message != "" {
			return fmt.Errorf("supervisor failed during startup: %s", message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			// Check once more at the deadline: the supervisor may have acquired its
			// lease while this process was waiting to be scheduled.
			if ready() {
				return nil
			}
			if message := failure(); message != "" {
				return fmt.Errorf("supervisor failed during startup: %s", message)
			}
			return fmt.Errorf("%w after %s", errStartupAckTimeout, timeout)
		case <-ticker.C:
		}
	}
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
	// A crashed previous supervisor may have left a stale PID in the local cache.
	// The lock was free above, so only a new controller can acknowledge this start.
	if e = p.DB.Set("pid", ""); e != nil {
		return e
	}
	if e = p.DB.Set("last_error", ""); e != nil {
		return e
	}
	if e = platform.Background(exe, []string{"start", "--foreground", "--repo", p.Root, "--home", p.Home}, p.Root, filepath.Join(logs, "supervisor.log")); e != nil {
		return e
	}
	if e = waitForSupervisorStart(cmd.Context(), startupAckTimeout, 200*time.Millisecond,
		func() bool { return active(p) && p.DB.Get("pid") != "" },
		func() string {
			if active(p) {
				return ""
			}
			return p.DB.Get("last_error")
		},
	); e != nil {
		if errors.Is(e, errStartupAckTimeout) && active(p) {
			fmt.Fprintln(cmd.OutOrStdout(), "Supervisor is still initializing; use aih status or aih watch to confirm startup.")
			return nil
		}
		return fmt.Errorf("%w; inspect %s", e, filepath.Join(logs, "supervisor.log"))
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Supervisor started. Use aih status or aih watch.")
	return nil
}
func checkUpdate(cmd *cobra.Command, p *engine.Project) {
	if p.Config.Lock.AutoUpdate == "off" {
		return
	}
	latest, e := update.Latest(cmd.Context(), config.ReleaseRepository(p.Config.Project.ReleaseRepo))
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
	if s.Capacity.TargetWriters == 0 {
		s.Capacity.TargetWriters = p.Config.Project.Scheduling.TargetWriters
		s.Capacity.MaxWriters = p.Config.Project.MaxWriters
		s.Capacity.MaxReaders = p.Config.Project.MaxReaders
		s.Capacity.MaxHeavyChecks = p.Config.Project.Resources.MaxHeavyChecks
		s.Capacity.MaxLightChecks = p.Config.Project.Resources.MaxLightChecks
		s.Capacity.GraceSeconds = p.Config.Project.Scheduling.UnderutilizationGraceSeconds
		s.Capacity.BacklogSource = p.Config.Project.Scheduling.BacklogSource
	}
	if s.Capacity.MaxHeavyChecks == 0 {
		s.Capacity.MaxHeavyChecks = p.Config.Project.Resources.MaxHeavyChecks
	}
	if s.Capacity.MaxLightChecks == 0 {
		s.Capacity.MaxLightChecks = p.Config.Project.Resources.MaxLightChecks
	}
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		machineHeavy := p.Machine.MaxHeavyChecks
		if machineHeavy == 0 {
			machineHeavy = 1
		}
		return enc.Encode(struct {
			*model.Snapshot
			MachineMaxHeavyChecks int `json:"machine_max_heavy_checks"`
		}{s, machineHeavy})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Project %s · durable revision %d (%s)\n", s.Project, s.Revision, shortSHA(h))
	fmt.Fprintf(cmd.OutOrStdout(), "Controller: %s · durable heartbeat %s · lease expires %s\n", s.Controller.Machine, s.Controller.Heartbeat.Format(time.RFC3339), s.Controller.Expires.Format(time.RFC3339))
	if active(p) {
		local := p.DB.Get(engine.LocalLeaseHeartbeatKey)
		if heartbeat, err := time.Parse(time.RFC3339Nano, local); err == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Local supervisor: active · local heartbeat %s (durable renewals are coalesced)\n", heartbeat.Format(time.RFC3339))
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "Local supervisor: active (cached state; inspect logs for progress)")
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Local supervisor: stopped")
	}
	if message := p.DB.Get("last_error"); message != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Last supervisor error:", message)
	}
	capacity := s.Capacity
	fmt.Fprintf(cmd.OutOrStdout(), "Writers: %d active / %d target / %d max\n", capacity.ActiveWriters, capacity.TargetWriters, capacity.MaxWriters)
	fmt.Fprintf(cmd.OutOrStdout(), "Readers: %d active / %d max\n", capacity.ActiveReaders, capacity.MaxReaders)
	heavy, light := 0, 0
	queued := map[string]bool{}
	for _, check := range capacity.Verification {
		if check.Phase == "queued" {
			queued[check.Task] = true
		}
		if check.Phase == "running" && check.Class == "heavy" {
			heavy++
		}
		if check.Phase == "running" && check.Class == "light" {
			light++
		}
	}
	machineHeavy := p.Machine.MaxHeavyChecks
	if machineHeavy == 0 {
		machineHeavy = 1
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Checks: %d heavy / %d project max / %d machine max, %d light / %d max\n", heavy, capacity.MaxHeavyChecks, machineHeavy, light, capacity.MaxLightChecks)
	for _, check := range capacity.Verification {
		at := check.QueuedAt
		if check.Phase == "running" {
			at = check.StartedAt
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %s %s check %q for %s (%s; %s)\n", check.Phase, check.Class, check.Check, check.Task, time.Since(at).Round(time.Second), map[string]string{"queued": "waiting for a verification slot", "running": "slot owned"}[check.Phase])
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Preflights: %d active\n", capacity.ActivePreflights)
	if capacity.ReasonCode != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Backfill: %s — %s (%s)\n", capacity.State, capacity.Reason, capacity.ReasonCode)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Backfill: %s\n", capacity.State)
	}
	if capacity.NextSafeWork != "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Next safe work:", capacity.NextSafeWork)
	}
	for _, t := range model.Ordered(s) {
		if blockers && t.State != model.Blocked {
			continue
		}
		status := string(t.State)
		if queued[t.ID] {
			status = "WAITING_CHECK_CAPACITY"
		}
		if t.Preflight != nil && (t.State == model.Ready || t.State == model.Fix) {
			switch t.Preflight.Phase {
			case "queued":
				status = "PREFLIGHT_QUEUED"
			case "waiting":
				status = "PREFLIGHT_WAITING"
			case "running":
				status = "PREFLIGHT_RUNNING"
			case "ready":
				status = "READY_TO_WRITE"
			}
		}
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
		if t.Verification != nil {
			route := "guarded native verification retry"
			if t.Verification.NativeOnly {
				route = "supervisor-native verification only"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  Verification route: %s (%s, attempt %d)\n", route, t.Verification.Environment, t.Verification.Attempts)
		}
		if t.State == model.Review {
			fmt.Fprintln(cmd.OutOrStdout(), "  Review: independent peer roles are active or waiting for bounded reader slots")
		}
	}
	if len(s.Runs) > 0 {
		const recentRunLimit = 10
		shown := map[int]bool{}
		for i, run := range s.Runs {
			if run.Outcome == "running" {
				shown[i] = true
			}
		}
		for i := len(s.Runs) - recentRunLimit; i < len(s.Runs); i++ {
			if i >= 0 {
				shown[i] = true
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Worker runs (active plus latest %d of %d; full history in status --json):\n", recentRunLimit, len(s.Runs))
		for i, run := range s.Runs {
			if !shown[i] {
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s %s: capability=%s effective_model=%s outcome=%s\n", run.Role, run.ID, run.Capability, run.EffectiveModel, run.Outcome)
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
		for rows.Next() {
			var id, msg string
			if rows.Scan(&id, &msg) == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Rejected local request %s: %s\n", id, msg)
			}
		}
		_ = rows.Close()
	}
	deadlineRows, deadlineErr := p.DB.DB.Query("SELECT at,task,kind,message FROM events WHERE kind IN ('worker_checkpoint_requested','worker_checkpoint_completed','worker_checkpoint_failed','worker_hard_timeout','worker_soft_timeout_idle') ORDER BY id DESC LIMIT 3")
	if deadlineErr == nil {
		for deadlineRows.Next() {
			var at, task, kind, message string
			if deadlineRows.Scan(&at, &task, &kind, &message) == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Worker deadline %s %s %s: %s\n", at, task, kind, message)
			}
		}
		_ = deadlineRows.Close()
	}
	reviewRows, reviewErr := p.DB.DB.Query("SELECT at,task,role,kind,message FROM events WHERE kind IN ('review_evidence_refresh_requested','review_evidence_refresh_completed','review_evidence_refresh_failed','review_finding_fix','human_decision_required') ORDER BY id DESC LIMIT 5")
	if reviewErr == nil {
		for reviewRows.Next() {
			var at, task, role, kind, message string
			if reviewRows.Scan(&at, &task, &role, &kind, &message) == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "Review lifecycle %s %s %s %s: %s\n", at, task, role, kind, message)
			}
		}
		_ = reviewRows.Close()
	}
	return e
}
func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func modelMappingWarnings(cfg config.Effective) []string {
	all, err := roles.Load(cfg.Files)
	if err != nil {
		return cfg.Project.ModelMappingWarnings()
	}
	capabilities := make(map[string]string, len(all))
	for name, role := range all {
		capabilities[name] = role.Capability
	}
	return cfg.Project.ModelMappingWarningsForRoles(capabilities)
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
			if re != nil || normalizedPolicy(string(b)) != normalizedPolicy(canonical) {
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
}
func addUpdate(root *cobra.Command, o *options) {
	root.AddCommand(&cobra.Command{Use: "self-update", Short: "Download and verify the latest compatible stable release", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, home, e := o.paths()
		if e != nil {
			return e
		}
		repo := config.ReleaseRepository("")
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
