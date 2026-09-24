package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

type Project struct {
	ID             string            `yaml:"project_id" json:"project_id"`
	Provider       string            `yaml:"provider" json:"provider"`
	Base           string            `yaml:"base_branch" json:"base_branch"`
	MaxWriters     int               `yaml:"max_parallel_writers" json:"max_parallel_writers"`
	MaxReaders     int               `yaml:"max_parallel_readers" json:"max_parallel_readers"`
	Resources      Resources         `yaml:"resources" json:"resources"`
	Models         map[string]string `yaml:"models" json:"models"`
	ProviderModels map[string]string `yaml:"provider_models" json:"provider_models"`
	Checks         []Check           `yaml:"checks" json:"checks"`
	VisualCapture  *VisualCapture    `yaml:"visual_capture,omitempty" json:"visual_capture,omitempty"`
	ReviewReuse    ReviewReuse       `yaml:"review_reuse,omitempty" json:"review_reuse,omitempty"`
	WorkerSeconds  int               `yaml:"worker_timeout_seconds" json:"worker_timeout_seconds"`
	LeaseSeconds   int               `yaml:"lease_seconds" json:"lease_seconds"`
	ReleaseRepo    string            `yaml:"release_repo" json:"release_repo"`
	Scheduling     Scheduling        `yaml:"scheduling" json:"scheduling"`
}
type Scheduling struct {
	TargetWriters                int    `yaml:"target_active_writers" json:"target_active_writers"`
	UnderutilizationGraceSeconds int    `yaml:"underutilization_grace_seconds" json:"underutilization_grace_seconds"`
	BacklogSource                string `yaml:"backlog_source" json:"backlog_source"`
}
type Resources struct {
	MaxHeavyChecks int `yaml:"max_heavy_checks" json:"max_heavy_checks"`
	MaxLightChecks int `yaml:"max_light_checks" json:"max_light_checks"`
}
type Check struct {
	Name      string   `yaml:"name" json:"name"`
	Command   []string `yaml:"command" json:"command"`
	Platforms []string `yaml:"platforms,omitempty" json:"platforms,omitempty"`
	Timeout   int      `yaml:"timeout_seconds" json:"timeout_seconds"`
	Class     string   `yaml:"class,omitempty" json:"class,omitempty"`
}

// VisualCapture declares the project adapter command. AIH supplies ephemeral
// TLS material, chooses no port itself, and owns the browser and gateway.
type VisualCapture struct {
	// Prepare optionally creates or verifies the adapter runtime in the
	// supervisor-owned detached checkout. It is run once per capture, before
	// Server, and never in a writer worktree.
	Prepare        []string              `yaml:"prepare,omitempty" json:"prepare,omitempty"`
	PrepareTimeout int                   `yaml:"prepare_timeout_seconds,omitempty" json:"prepare_timeout_seconds,omitempty"`
	Server         []string              `yaml:"server" json:"server"`
	Timeout        int                   `yaml:"timeout_seconds" json:"timeout_seconds"`
	Targets        []VisualCaptureTarget `yaml:"targets,omitempty" json:"targets,omitempty"`
}

// VisualCaptureTarget is a same-origin application route and the viewport at
// which AIH captures it. It deliberately has no browser, profile, or URL host
// controls: those remain supervisor-owned.
type VisualCaptureTarget struct {
	ID     string `yaml:"id" json:"id"`
	Path   string `yaml:"path" json:"path"`
	Width  int    `yaml:"width" json:"width"`
	Height int    `yaml:"height" json:"height"`
}

const (
	maxVisualCaptureTargets = 8
	maxVisualTargetPixels   = 4 << 20
	maxVisualCapturePixels  = 16 << 20
)

var visualTargetID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

var windowsReservedVisualTargetID = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true, "com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true, "lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// CaptureTargets returns the legacy browser capture when configuration omits targets.
func (v *VisualCapture) CaptureTargets() []VisualCaptureTarget {
	if len(v.Targets) == 0 {
		return []VisualCaptureTarget{{ID: "desktop", Path: "/", Width: 1280, Height: 720}}
	}
	return v.Targets
}

// ReviewReuse is opt-in policy for the exceptionally narrow review reuse
// path. Entries name inert, project-specific text-data locations; ordinary
// documentation, source, and agent instructions are never implicitly trusted.
type ReviewReuse struct {
	SecurityDataOnlyPaths []string `yaml:"security_data_only_paths,omitempty" json:"security_data_only_paths,omitempty"`
}
type Policy struct {
	ImplementationRetries int `yaml:"implementation_retries"`
	ReviewCycles          int `yaml:"review_fix_cycles"`
	QACycles              int `yaml:"qa_fix_cycles"`
	DesignCycles          int `yaml:"design_fix_cycles"`
}
type Lock struct {
	Major      int    `yaml:"major"`
	Minimum    string `yaml:"minimum"`
	Rules      int    `yaml:"engineering_rules_version"`
	Schema     int    `yaml:"state_schema"`
	Channel    string `yaml:"update_channel"`
	AutoUpdate string `yaml:"auto_update"`
}
type Machine struct {
	ID             string `yaml:"machine_id"`
	Platform       string `yaml:"platform"`
	Provider       string `yaml:"default_provider"`
	MaxHeavyChecks int    `yaml:"max_heavy_checks,omitempty"`
}
type Registration struct {
	ProjectID  string `json:"project_id"`
	Repository string `json:"repository"`
	Remote     string `json:"remote"`
	Root       string `json:"root"`
}
type Effective struct {
	BaseSHA string
	Project Project
	Policy  Policy
	Lock    Lock
	Files   map[string]string
	Hash    string
}

// ModelResolution keeps the configured capability separate from what AIH can
// actually request from a provider. ProviderDefault is deliberate evidence: AIH
// did not guess a provider-specific model identifier and omitted --model.
type ModelResolution struct {
	Capability     string `json:"capability"`
	EffectiveModel string `json:"effective_model"`
	RequestModel   string `json:"-"`
}

const ProviderDefaultModel = "provider-default"

func (p Project) ResolveModel(role, fallback string) ModelResolution {
	capability := fallback
	if configured := p.Models[role]; configured != "" {
		capability = configured
	}
	if requested := p.ProviderModels[capability]; requested != "" {
		return ModelResolution{Capability: capability, EffectiveModel: requested, RequestModel: requested}
	}
	return ModelResolution{Capability: capability, EffectiveModel: ProviderDefaultModel}
}

// ModelMappingWarnings identifies every configured role whose provider model is
// implicit. A complete mapping is explicit even when several tiers intentionally
// use the same model ID.
func (p Project) ModelMappingWarnings() []string {
	return p.ModelMappingWarningsForRoles(map[string]string{
		"orchestrator": "strong", "implementer": "normal", "reviewer": "strong", "qa": "normal", "designer": "strong", "security": "strong", "advisor": "strongest",
	})
}

// ModelMappingWarningsForRoles applies the same policy to built-in and loaded
// custom roles. Roles are the source of capability defaults; Project.Models can
// override either kind by role name.
func (p Project) ModelMappingWarningsForRoles(roleCapabilities map[string]string) []string {
	missing := map[string][]string{}
	roles := make([]string, 0, len(roleCapabilities))
	for role := range roleCapabilities {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		resolved := p.ResolveModel(role, roleCapabilities[role])
		if resolved.EffectiveModel == ProviderDefaultModel {
			missing[resolved.Capability] = append(missing[resolved.Capability], role)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	tiers := make([]string, 0, len(missing))
	for tier := range missing {
		tiers = append(tiers, tier)
	}
	sort.Strings(tiers)
	warnings := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		warnings = append(warnings, fmt.Sprintf("provider_models omits capability %q; roles %s use provider-default. Add an explicit model ID for this tier (identical IDs are allowed when intentional).", tier, strings.Join(missing[tier], ", ")))
	}
	return warnings
}

func Defaults() Project {
	return Project{ID: model.ID(), Provider: "codex", Base: "main", MaxWriters: 3, MaxReaders: 2, Resources: Resources{MaxHeavyChecks: 1, MaxLightChecks: 2}, Models: map[string]string{"orchestrator": "strong", "implementer": "normal", "reviewer": "strong", "qa": "normal", "designer": "strong", "security": "strong", "advisor": "strongest"}, ProviderModels: map[string]string{}, WorkerSeconds: 900, LeaseSeconds: 180, ReleaseRepo: UpstreamRepository, Scheduling: Scheduling{TargetWriters: 2, UnderutilizationGraceSeconds: 30, BacklogSource: "queued_objectives"}}
}
func DefaultPolicy() Policy { return Policy{2, 3, 3, 3} }
func DefaultLock() Lock {
	return Lock{1, model.Version, model.RulesVersion, model.StateSchema, "stable", "notify"}
}
func Decode(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if e := dec.Decode(out); e != nil {
		return e
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		return errors.New("configuration must contain exactly one YAML document")
	}
	return nil
}
func DecodeProject(data []byte, out *Project) error {
	hasScheduling := regexp.MustCompile(`(?m)^scheduling\s*:`).Match(data)
	hasResources := regexp.MustCompile(`(?m)^resources\s*:`).Match(data)
	if err := Decode(data, out); err != nil {
		return err
	}
	if !hasScheduling {
		out.Scheduling = Scheduling{TargetWriters: 2, UnderutilizationGraceSeconds: 30, BacklogSource: "queued_objectives"}
		if out.MaxWriters > 0 && out.Scheduling.TargetWriters > out.MaxWriters {
			out.Scheduling.TargetWriters = out.MaxWriters
		}
	}
	if !hasResources {
		out.Resources = Resources{MaxHeavyChecks: 1, MaxLightChecks: 2}
	}
	return nil
}
func Parse(files map[string]string) (Effective, error) {
	e := Effective{Project: Defaults(), Policy: DefaultPolicy(), Lock: DefaultLock(), Files: files}
	e.Project.ID = "" // Identity must be committed, never silently generated on load.
	for name, out := range map[string]any{".aih/project.yaml": &e.Project, ".aih/policies.yaml": &e.Policy, ".aih/harness.lock": &e.Lock} {
		data, ok := files[name]
		if !ok {
			return e, fmt.Errorf("canonical base missing %s; commit and push AIH configuration first", name)
		}
		var err error
		if name == ".aih/project.yaml" {
			err = DecodeProject([]byte(data), &e.Project)
		} else {
			err = Decode([]byte(data), out)
		}
		if err != nil {
			return e, fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := e.Project.Validate(); err != nil {
		return e, err
	}
	if e.Lock.Major != 1 || e.Lock.Schema > model.StateSchema || e.Lock.Rules > model.RulesVersion {
		return e, errors.New("project requires an incompatible AIH version/schema")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(e.Lock.Minimum) || e.Lock.Channel != "stable" || (e.Lock.AutoUpdate != "notify" && e.Lock.AutoUpdate != "off") {
		return e, errors.New("lock requires a stable minimum version, stable channel, and auto_update notify or off")
	}
	if CompareVersion(model.Version, e.Lock.Minimum) < 0 {
		return e, fmt.Errorf("project requires AIH >= %s", e.Lock.Minimum)
	}
	if e.Policy.ImplementationRetries < 0 || e.Policy.ReviewCycles < 0 || e.Policy.QACycles < 0 || e.Policy.DesignCycles < 0 {
		return e, errors.New("retry budgets cannot be negative")
	}
	b, _ := json.Marshal(files)
	h := sha256.Sum256(b)
	e.Hash = hex.EncodeToString(h[:])
	return e, nil
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`)

func (p Project) Validate() error {
	if p.ReleaseRepo != "" && !ValidRepository(p.ReleaseRepo) {
		return errors.New("release_repo must be an owner/repository name")
	}
	if !validID.MatchString(p.ID) {
		return errors.New("invalid project_id")
	}
	if p.Provider != "codex" && p.Provider != "claude-code" {
		return errors.New("provider must be codex or claude-code")
	}
	if p.Base != "main" {
		return errors.New("V1 canonical base_branch must be main")
	}
	if p.MaxWriters < 1 || p.MaxWriters > 3 || p.MaxReaders < 1 || p.MaxReaders > 8 {
		return errors.New("writer limit must be 1..3 and reader limit 1..8")
	}
	if p.Resources.MaxHeavyChecks < 1 || p.Resources.MaxHeavyChecks > 8 || p.Resources.MaxLightChecks < 1 || p.Resources.MaxLightChecks > 8 {
		return errors.New("check resource limits must be 1..8")
	}
	if p.Scheduling.TargetWriters < 1 || p.Scheduling.TargetWriters > p.MaxWriters {
		return errors.New("target_active_writers must be between 1 and max_parallel_writers")
	}
	if p.Scheduling.UnderutilizationGraceSeconds < 0 || p.Scheduling.UnderutilizationGraceSeconds > 3600 {
		return errors.New("underutilization_grace_seconds must be between 0 and 3600")
	}
	if p.Scheduling.BacklogSource != "queued_objectives" {
		return errors.New("V1 scheduling backlog_source must be queued_objectives")
	}
	if p.WorkerSeconds < 10 || p.LeaseSeconds < 60 {
		return errors.New("worker timeout must be >=10s and lease >=60s")
	}
	if len(p.ReviewReuse.SecurityDataOnlyPaths) > 8 {
		return errors.New("review_reuse may declare at most 8 data-only paths")
	}
	for _, pattern := range p.ReviewReuse.SecurityDataOnlyPaths {
		if !regexp.MustCompile(`^[a-zA-Z0-9_./*?-]{1,160}$`).MatchString(pattern) || strings.HasPrefix(pattern, ".") || strings.Contains(pattern, "..") || !strings.HasSuffix(strings.ToLower(pattern), ".txt") {
			return errors.New("review_reuse data-only paths must be safe relative .txt globs")
		}
	}
	for _, c := range p.Checks {
		if c.Name == "" || len(c.Command) == 0 || c.Timeout <= 0 {
			return errors.New("each check needs name, command argv, and positive timeout_seconds")
		}
		if strings.TrimSpace(c.Command[0]) == "" {
			return errors.New("verification executable cannot be empty")
		}
		if c.Class != "" && c.Class != "heavy" && c.Class != "light" {
			return errors.New("check class must be heavy or light")
		}
		for _, platform := range c.Platforms {
			if platform != "windows" && platform != "darwin" && platform != "linux" {
				return errors.New("unknown verification platform")
			}
		}
	}
	if p.VisualCapture != nil {
		if len(p.VisualCapture.Server) == 0 || strings.TrimSpace(p.VisualCapture.Server[0]) == "" {
			return errors.New("visual_capture needs a server command argv")
		}
		if p.VisualCapture.Timeout < 1 || p.VisualCapture.Timeout > 300 {
			return errors.New("visual_capture timeout_seconds must be between 1 and 300")
		}
		if len(p.VisualCapture.Prepare) == 0 && p.VisualCapture.PrepareTimeout != 0 {
			return errors.New("visual_capture prepare_timeout_seconds requires a prepare command argv")
		}
		if len(p.VisualCapture.Prepare) > 0 {
			if strings.TrimSpace(p.VisualCapture.Prepare[0]) == "" {
				return errors.New("visual_capture prepare needs a command argv")
			}
			if p.VisualCapture.PrepareTimeout != 0 && (p.VisualCapture.PrepareTimeout < 1 || p.VisualCapture.PrepareTimeout > 300) {
				return errors.New("visual_capture prepare_timeout_seconds must be between 1 and 300")
			}
		}
		if len(p.VisualCapture.Targets) > maxVisualCaptureTargets {
			return fmt.Errorf("visual_capture targets must contain at most %d entries", maxVisualCaptureTargets)
		}
		seen := map[string]bool{}
		pixels := 0
		for _, target := range p.VisualCapture.Targets {
			filenameID := strings.ToLower(target.ID)
			if !visualTargetID.MatchString(target.ID) || seen[filenameID] || windowsReservedVisualTargetID[filenameID] {
				return errors.New("visual_capture target IDs must be unique case-insensitive safe filenames")
			}
			seen[filenameID] = true
			u, err := url.Parse(target.Path)
			if err != nil || target.Path == "" || !strings.HasPrefix(target.Path, "/") || strings.HasPrefix(target.Path, "//") || u.IsAbs() || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return errors.New("visual_capture target path must be a same-origin absolute path without host, query, or fragment")
			}
			if target.Width < 1 || target.Height < 1 || target.Width > 4096 || target.Height > 4096 || target.Width*target.Height > maxVisualTargetPixels {
				return errors.New("visual_capture target viewport exceeds the allowed dimensions")
			}
			pixels += target.Width * target.Height
			if pixels > maxVisualCapturePixels {
				return errors.New("visual_capture target viewports exceed the aggregate pixel limit")
			}
		}
	}
	for _, capability := range p.Models {
		if capability != "normal" && capability != "strong" && capability != "strongest" {
			return errors.New("unknown model capability tier")
		}
	}
	for capability := range p.ProviderModels {
		if capability != "normal" && capability != "strong" && capability != "strongest" {
			return errors.New("unknown provider model capability mapping")
		}
	}
	return nil
}
func Home(override string) (string, error) {
	if override == "" {
		override = os.Getenv("AIH_HOME")
	}
	if override == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		override = filepath.Join(h, ".aih")
	}
	return filepath.Abs(override)
}
func Install(home string) (Machine, error) {
	if e := os.MkdirAll(filepath.Join(home, "projects"), 0700); e != nil {
		return Machine{}, e
	}
	p := filepath.Join(home, "machine.yaml")
	m := Machine{ID: model.ID(), Platform: runtime.GOOS, Provider: "codex", MaxHeavyChecks: 1}
	b, e := os.ReadFile(p)
	if e == nil {
		e = Decode(b, &m)
		if m.MaxHeavyChecks == 0 {
			m.MaxHeavyChecks = 1
		}
		if m.MaxHeavyChecks < 1 || m.MaxHeavyChecks > 8 {
			return m, errors.New("machine max_heavy_checks must be 1..8")
		}
		return m, e
	}
	if !os.IsNotExist(e) {
		return m, e
	}
	b, _ = yaml.Marshal(m)
	f, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(e) {
		return Install(home)
	}
	if e != nil {
		return m, e
	}
	_, e = f.Write(b)
	ce := f.Close()
	if e != nil {
		return m, e
	}
	return m, ce
}

// MachineConfig reads optional machine capacity without registering or
// initializing an AIH installation. Callers that only need shared resource
// coordination use the conservative default when this machine has not been
// installed yet.
func MachineConfig(home string) (Machine, error) {
	m := Machine{Platform: runtime.GOOS, MaxHeavyChecks: 1}
	b, e := os.ReadFile(filepath.Join(home, "machine.yaml"))
	if os.IsNotExist(e) {
		return m, nil
	}
	if e != nil {
		return m, e
	}
	if e = Decode(b, &m); e != nil {
		return m, e
	}
	if m.MaxHeavyChecks == 0 {
		m.MaxHeavyChecks = 1
	}
	if m.MaxHeavyChecks < 1 || m.MaxHeavyChecks > 8 {
		return m, errors.New("machine max_heavy_checks must be 1..8")
	}
	return m, nil
}
func ProjectDir(home, id string) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New("invalid project identity")
	}
	return filepath.Join(home, "projects", id), nil
}
func Register(home string, r Registration) error {
	d, e := ProjectDir(home, r.ProjectID)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(d, 0700); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	return os.WriteFile(filepath.Join(d, "registration.json"), b, 0600)
}
func GitHubRepo(remote string) (string, error) {
	if strings.HasPrefix(remote, "ssh://git@github.com/") {
		remote = "https://github.com/" + strings.TrimPrefix(remote, "ssh://git@github.com/")
	}
	if strings.HasPrefix(remote, "git@github.com:") {
		remote = "https://github.com/" + strings.TrimPrefix(remote, "git@github.com:")
	}
	u, e := url.Parse(remote)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("origin must be a github.com HTTPS or SSH URL without embedded credentials")
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !ValidRepository(p) {
		return "", errors.New("invalid GitHub repository URL")
	}
	return p, nil
}
func DetectChecks(root string) []Check {
	if _, e := os.Stat(filepath.Join(root, "go.mod")); e == nil {
		return []Check{{Name: "unit tests", Command: []string{"go", "test", "./..."}, Timeout: 600, Class: "heavy"}, {Name: "vet", Command: []string{"go", "vet", "./..."}, Timeout: 300, Class: "heavy"}}
	}
	if b, e := os.ReadFile(filepath.Join(root, "package.json")); e == nil {
		var p struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &p) == nil {
			var checks []Check
			for _, n := range []string{"lint", "typecheck", "test", "build"} {
				if _, ok := p.Scripts[n]; ok {
					checks = append(checks, Check{Name: n, Command: []string{"npm", "run", n}, Timeout: 600, Class: "heavy"})
				}
			}
			return checks
		}
	}
	if _, e := os.Stat(filepath.Join(root, "Cargo.toml")); e == nil {
		return []Check{{Name: "tests", Command: []string{"cargo", "test"}, Timeout: 600, Class: "heavy"}}
	}
	return nil
}
func CompareVersion(a, b string) int {
	var av, bv [3]int
	_, ae := fmt.Sscanf(strings.TrimPrefix(a, "v"), "%d.%d.%d", &av[0], &av[1], &av[2])
	_, be := fmt.Sscanf(strings.TrimPrefix(b, "v"), "%d.%d.%d", &bv[0], &bv[1], &bv[2])
	if ae != nil || be != nil {
		return -1
	}
	for i := range av {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	return 0
}
