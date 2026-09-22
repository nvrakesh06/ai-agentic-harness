// Package provider owns the two supported CLI protocols. Every invocation is
// disposable; restarting never requires a provider session ID.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	Schema       int              `json:"schema_version"`
	Status       string           `json:"status"`
	Summary      string           `json:"summary"`
	Question     string           `json:"question"`
	ChangedAreas []string         `json:"changed_areas"`
	Tests        []string         `json:"tests_run"`
	Risks        []string         `json:"remaining_risks"`
	Findings     []model.Finding  `json:"findings"`
	Plan         []model.PlanTask `json:"plan"`
}
type Request struct {
	Directory, Runtime, Prompt, Role, Model string
	Write                                   bool
	Timeout                                 time.Duration
}
type Provider interface {
	Name() string
	Validate(context.Context) error
	Run(context.Context, Request) (Result, error)
}
type CLI struct{ Kind, Executable string }

func New(name string) Provider {
	exe := "codex"
	variable := "CODEX_BINARY"
	if name == "claude-code" {
		exe = "claude"
		variable = "CLAUDE_BINARY"
	}
	if configured := os.Getenv(variable); configured != "" {
		exe = configured
	}
	return CLI{name, exe}
}
func (c CLI) Name() string { return c.Kind }
func (c CLI) Validate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"--help"}
	if c.Kind == "codex" {
		args = []string{"exec", "--help"}
	}
	out, e := platform.Run(ctx, "", nil, "", c.Executable, args...)
	if e != nil {
		variable := "CODEX_BINARY"
		if c.Kind == "claude-code" {
			variable = "CLAUDE_BINARY"
		}
		return fmt.Errorf("%s CLI unavailable: install it on PATH or set %s to its executable path; see docs/AGENTS_SETUP.md: %w", c.Kind, variable, e)
	}
	flags := []string{"--json-schema", "--output-format", "--permission-mode", "--safe-mode"}
	if c.Kind == "codex" {
		flags = []string{"--output-schema", "--output-last-message", "--sandbox"}
	}
	for _, flag := range flags {
		if !strings.Contains(out, flag) {
			return fmt.Errorf("%s lacks required flag %s; update the provider CLI", c.Kind, flag)
		}
	}
	return nil
}

// Authentication probes do not run a model or expose account/token output.
func CheckAuthentication(ctx context.Context, name string) error {
	c := New(name).(CLI)
	args := []string{"login", "status"}
	if name == "claude-code" {
		args = []string{"auth", "status"}
	} else if name != "codex" {
		return errors.New("unsupported provider")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := platform.Run(ctx, "", nil, "", c.Executable, args...); err != nil {
		return fmt.Errorf("%s authentication unavailable; follow docs/AGENTS_SETUP.md as this OS user", name)
	}
	return nil
}
func (c CLI) Run(parent context.Context, r Request) (Result, error) {
	var result Result
	if e := os.MkdirAll(r.Runtime, 0700); e != nil {
		return result, e
	}
	schema := Schema()
	schemaPath := filepath.Join(r.Runtime, "result.schema.json")
	resultPath := filepath.Join(r.Runtime, "result.json")
	if e := os.WriteFile(schemaPath, []byte(schema), 0600); e != nil {
		return result, e
	}
	args := []string{}
	if c.Kind == "codex" {
		sandbox := "read-only"
		if r.Write {
			sandbox = "workspace-write"
		}
		args = []string{"exec", "--json", "--sandbox", sandbox, "--output-schema", schemaPath, "--output-last-message", resultPath, "-c", "approval_policy=\"never\"", "-c", "project_doc_max_bytes=0"}
		if r.Model != "" {
			args = append(args, "--model", r.Model)
		}
		args = append(args, "-")
	} else if c.Kind == "claude-code" {
		// Safe mode prevents stale CLAUDE.md, hooks, plugins and MCP tools from
		// silently changing this run's canonical policy or tool permissions.
		args = []string{"--print", "--safe-mode", "--output-format", "json", "--json-schema", schema, "--permission-mode", "dontAsk", "--no-session-persistence", "--tools", "Read,Glob,Grep"}
		if r.Write {
			args[len(args)-1] = "Read,Glob,Grep,Edit,Write"
			args = append(args, "--allowedTools", "Read,Glob,Grep,Edit,Write")
		}
		if r.Model != "" {
			args = append(args, "--model", r.Model)
		}
	} else {
		return result, errors.New("unsupported provider")
	}
	env := []string{}
	for _, v := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(v, "=", 2)[0])
		if k == "GH_TOKEN" || k == "GITHUB_TOKEN" || strings.HasPrefix(k, "GIT_CONFIG") {
			continue
		}
		env = append(env, v)
	}
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	out, e := platform.Run(ctx, r.Directory, env, r.Prompt, c.Executable, args...)
	// Keep diagnostics local and redact known credential formats. Prompts are
	// not written to logs; final results are scanned again before publication.
	_ = os.WriteFile(filepath.Join(r.Runtime, "output.log"), []byte(safety.Redact(out)), 0600)
	if e != nil {
		return result, fmt.Errorf("%s invocation failed: %w", c.Kind, e)
	}
	if c.Kind == "codex" {
		b, re := os.ReadFile(resultPath)
		if re != nil {
			return result, fmt.Errorf("missing Codex structured result: %w", re)
		}
		out = string(b)
	} else {
		var envelope struct {
			Structured json.RawMessage `json:"structured_output"`
			Result     string          `json:"result"`
			IsError    bool            `json:"is_error"`
		}
		if e = json.Unmarshal([]byte(out), &envelope); e != nil {
			return result, fmt.Errorf("malformed Claude envelope: %w", e)
		}
		if envelope.IsError {
			return result, errors.New("Claude returned an error result")
		}
		if len(envelope.Structured) > 0 {
			out = string(envelope.Structured)
		} else {
			out = envelope.Result
		}
	}
	return Parse(out, r.Role)
}
func Parse(out, role string) (Result, error) {
	var r Result
	if e := safety.Check(out); e != nil {
		return r, e
	}
	d := json.NewDecoder(strings.NewReader(out))
	d.DisallowUnknownFields()
	if e := d.Decode(&r); e != nil {
		return r, fmt.Errorf("malformed structured result: %w", e)
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return r, errors.New("trailing data after worker result")
	}
	if r.Schema != 1 {
		return r, errors.New("worker result schema must be 1")
	}
	if r.Status != "completed" && r.Status != "blocked" && r.Status != "in_progress" && r.Status != "failed" {
		return r, errors.New("invalid worker status")
	}
	if r.Summary == "" {
		return r, errors.New("worker summary is required")
	}
	if r.Status == "blocked" && strings.TrimSpace(r.Question) == "" && role != "implementer" {
		return r, errors.New("blocked advisory result requires a question")
	}
	for _, f := range r.Findings {
		if !roles.Severity(f.Severity) || f.Reason == "" {
			return r, errors.New("malformed finding")
		}
	}
	if role == "orchestrator" && r.Status == "completed" {
		if e := model.ValidatePlan(r.Plan); e != nil {
			return r, e
		}
	}
	return r, nil
}
func Schema() string {
	str := map[string]any{"type": "string"}
	array := func(item any) any { return map[string]any{"type": "array", "items": item} }
	nonEmptyString := map[string]any{"type": "string", "minLength": 1}
	nonEmptyArray := func(item any) any { return map[string]any{"type": "array", "items": item, "minItems": 1} }
	obj := func(props map[string]any) any {
		required := []string{}
		for k := range props {
			required = append(required, k)
		}
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	finding := obj(map[string]any{"severity": map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low", "nit"}}, "category": str, "location": str, "reason": str, "suggested_resolution": str})
	risk := map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}}
	key := map[string]any{"type": "string", "pattern": model.PlanKeyPattern}
	task := obj(map[string]any{"key": key, "title": nonEmptyString, "objective": nonEmptyString, "acceptance": nonEmptyArray(str), "dependencies": array(str), "areas": nonEmptyArray(str), "conflict_domains": nonEmptyArray(str), "risk": risk, "ui": map[string]any{"type": "boolean"}, "security": map[string]any{"type": "boolean"}, "roles": array(str)})
	// All roles share this output schema and non-completed orchestrator results may
	// have no plan. Parse applies the 1-task minimum to completed orchestrator
	// results; the schema can still reject oversized plans and malformed tasks.
	plan := map[string]any{"type": "array", "items": task, "maxItems": model.MaxPlanTasks}
	schema := obj(map[string]any{"schema_version": map[string]any{"type": "integer", "const": 1}, "status": map[string]any{"type": "string", "enum": []string{"completed", "blocked", "in_progress", "failed"}}, "summary": str, "question": str, "changed_areas": array(str), "tests_run": array(str), "remaining_risks": array(str), "findings": array(finding), "plan": plan})
	b, _ := json.Marshal(schema)
	return string(b)
}
