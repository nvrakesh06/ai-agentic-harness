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
	"strings"
)

type Project struct {
	ID             string            `yaml:"project_id" json:"project_id"`
	Provider       string            `yaml:"provider" json:"provider"`
	Base           string            `yaml:"base_branch" json:"base_branch"`
	MaxWriters     int               `yaml:"max_parallel_writers" json:"max_parallel_writers"`
	MaxReaders     int               `yaml:"max_parallel_readers" json:"max_parallel_readers"`
	Models         map[string]string `yaml:"models" json:"models"`
	ProviderModels map[string]string `yaml:"provider_models" json:"provider_models"`
	Checks         []Check           `yaml:"checks" json:"checks"`
	WorkerSeconds  int               `yaml:"worker_timeout_seconds" json:"worker_timeout_seconds"`
	LeaseSeconds   int               `yaml:"lease_seconds" json:"lease_seconds"`
	ReleaseRepo    string            `yaml:"release_repo" json:"release_repo"`
}
type Check struct {
	Name      string   `yaml:"name" json:"name"`
	Command   []string `yaml:"command" json:"command"`
	Platforms []string `yaml:"platforms,omitempty" json:"platforms,omitempty"`
	Timeout   int      `yaml:"timeout_seconds" json:"timeout_seconds"`
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
	ID       string `yaml:"machine_id"`
	Platform string `yaml:"platform"`
	Provider string `yaml:"default_provider"`
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

func Defaults() Project {
	return Project{ID: model.ID(), Provider: "codex", Base: "main", MaxWriters: 3, MaxReaders: 2, Models: map[string]string{"orchestrator": "strong", "implementer": "normal", "reviewer": "strong", "qa": "normal", "designer": "strong", "security": "strong", "advisor": "strongest"}, ProviderModels: map[string]string{}, WorkerSeconds: 900, LeaseSeconds: 180, ReleaseRepo: "nvrakesh06/ai-agentic-harness"}
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
func Parse(files map[string]string) (Effective, error) {
	e := Effective{Project: Defaults(), Policy: DefaultPolicy(), Lock: DefaultLock(), Files: files}
	e.Project.ID = "" // Identity must be committed, never silently generated on load.
	for name, out := range map[string]any{".aih/project.yaml": &e.Project, ".aih/policies.yaml": &e.Policy, ".aih/harness.lock": &e.Lock} {
		data, ok := files[name]
		if !ok {
			return e, fmt.Errorf("canonical base missing %s; commit and push AIH configuration first", name)
		}
		if err := Decode([]byte(data), out); err != nil {
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
	if p.WorkerSeconds < 10 || p.LeaseSeconds < 60 {
		return errors.New("worker timeout must be >=10s and lease >=60s")
	}
	for _, c := range p.Checks {
		if c.Name == "" || len(c.Command) == 0 || c.Timeout <= 0 {
			return errors.New("each check needs name, command argv, and positive timeout_seconds")
		}
		if strings.TrimSpace(c.Command[0]) == "" {
			return errors.New("verification executable cannot be empty")
		}
		for _, platform := range c.Platforms {
			if platform != "windows" && platform != "darwin" && platform != "linux" {
				return errors.New("unknown verification platform")
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
	m := Machine{model.ID(), runtime.GOOS, "codex"}
	b, e := os.ReadFile(p)
	if e == nil {
		e = Decode(b, &m)
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
	if strings.HasPrefix(remote, "git@github.com:") {
		remote = "https://github.com/" + strings.TrimPrefix(remote, "git@github.com:")
	}
	u, e := url.Parse(remote)
	if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("origin must be a github.com HTTPS or SSH URL without embedded credentials")
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if len(strings.Split(p, "/")) != 2 {
		return "", errors.New("invalid GitHub repository URL")
	}
	return p, nil
}
func DetectChecks(root string) []Check {
	if _, e := os.Stat(filepath.Join(root, "go.mod")); e == nil {
		return []Check{{"unit tests", []string{"go", "test", "./..."}, nil, 600}, {"vet", []string{"go", "vet", "./..."}, nil, 300}}
	}
	if b, e := os.ReadFile(filepath.Join(root, "package.json")); e == nil {
		var p struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &p) == nil {
			var checks []Check
			for _, n := range []string{"lint", "typecheck", "test", "build"} {
				if _, ok := p.Scripts[n]; ok {
					checks = append(checks, Check{n, []string{"npm", "run", n}, nil, 600})
				}
			}
			return checks
		}
	}
	if _, e := os.Stat(filepath.Join(root, "Cargo.toml")); e == nil {
		return []Check{{"tests", []string{"cargo", "test"}, nil, 600}}
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
