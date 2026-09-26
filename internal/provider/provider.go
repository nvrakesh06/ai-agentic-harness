// Package provider owns the two supported CLI protocols. Every invocation is
// disposable; restarting never requires a provider session ID.
package provider

import (
	"context"
	"crypto/sha256"
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
	"sort"
	"strings"
	"time"
)

type Result struct {
	Schema                   int              `json:"schema_version"`
	Status                   string           `json:"status"`
	Summary                  string           `json:"summary"`
	Question                 string           `json:"question"`
	ChangedAreas             []string         `json:"changed_areas"`
	Tests                    []string         `json:"tests_run"`
	Risks                    []string         `json:"remaining_risks"`
	Findings                 []model.Finding  `json:"findings"`
	Plan                     []model.PlanTask `json:"plan"`
	RecoveredDeadlineHandoff bool             `json:"-"`
}
type Request struct {
	Directory, Runtime, Scratch, Prompt, Role, Model string
	Write                                            bool
	Timeout                                          time.Duration
}
type Provider interface {
	Name() string
	Validate(context.Context) error
	Run(context.Context, Request) (Result, error)
}
type CLI struct{ Kind, Executable string }

type InvocationError struct {
	Cause        error
	LastActivity time.Time
	OutputBytes  int
	Failure      FailureClass
	Rejection    string
}

func (e *InvocationError) Error() string { return e.Cause.Error() }
func (e *InvocationError) Unwrap() error { return e.Cause }

// FailureClass identifies a provider-side failure without retaining provider
// diagnostics in durable task state. It is deliberately small: task routing
// must not infer an authentication outage from arbitrary model or repository
// text.
type FailureClass string

const (
	FailureUnknown         FailureClass = ""
	FailureAuthentication  FailureClass = "authentication"
	FailureRequestRejected FailureClass = "request_rejected"

	// RejectionInvalidJSONSchema is the only request rejection currently
	// recognized from a documented provider error envelope. New codes need an
	// explicit classifier and admission scope; free-form error text is never a
	// durable provider hold.
	RejectionInvalidJSONSchema = "invalid_json_schema"
)

// IsAuthenticationFailure reports only a typed classification made by the
// supported provider adapter from a provider error record.
func IsAuthenticationFailure(err error) bool {
	var invocation *InvocationError
	return errors.As(err, &invocation) && invocation.Failure == FailureAuthentication
}

// RequestRejection reports a typed, documented request rejection without
// exposing the provider diagnostic. The returned code is safe to persist as a
// bounded admission identity component.
func RequestRejection(err error) (string, bool) {
	var invocation *InvocationError
	if !errors.As(err, &invocation) || invocation.Failure != FailureRequestRejected || invocation.Rejection == "" {
		return "", false
	}
	return invocation.Rejection, true
}

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
	if r.Scratch != "" {
		workspace, e := resolvedPath(r.Directory)
		if e != nil {
			return result, e
		}
		scratch, e := resolvedPath(r.Scratch)
		if e != nil {
			return result, e
		}
		rel, e := filepath.Rel(workspace, scratch)
		if e != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return result, errors.New("worker scratch must be outside the source worktree")
		}
		if e = os.MkdirAll(filepath.Join(scratch, "npm-cache"), 0700); e != nil {
			return result, e
		}
		r.Scratch = scratch
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
		if r.Role == "implementer" {
			// The public Codex CLI feature control disables the stable multi-agent
			// feature for this invocation. The supervisor owns AIH delegation.
			args = append(args, "--disable", "multi_agent")
		}
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
	if r.Scratch != "" {
		npmCache := filepath.Join(r.Scratch, "npm-cache")
		env = append(env,
			"AIH_SCRATCH="+r.Scratch,
			"TMP="+r.Scratch,
			"TEMP="+r.Scratch,
			"TMPDIR="+r.Scratch,
			"npm_config_cache="+npmCache,
			"NPM_CONFIG_CACHE="+npmCache,
		)
	}
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	observed, e := platform.RunObserved(ctx, r.Directory, env, r.Prompt, c.Executable, args...)
	diagnosticOutput := observed.Output
	// Keep diagnostics local and redact known credential formats. Prompts are
	// not written to logs; final results are scanned again before publication.
	_ = os.WriteFile(filepath.Join(r.Runtime, "output.log"), []byte(safety.Redact(diagnosticOutput)), 0600)
	if e != nil {
		if errors.Is(e, context.DeadlineExceeded) {
			if recovered, recoveredErr := c.recoverInProgress(observed.Stdout, resultPath, r.Role); recoveredErr == nil && recovered.Status == "in_progress" {
				recovered.RecoveredDeadlineHandoff = true
				return recovered, nil
			}
		}
		return result, c.invocationError(e, observed)
	}
	out := observed.Stdout
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
			return result, c.invocationError(errors.New("Claude returned an error result"), observed)
		}
		if len(envelope.Structured) > 0 {
			out = string(envelope.Structured)
		} else {
			out = envelope.Result
		}
	}
	return Parse(out, r.Role)
}

func (c CLI) invocationError(cause error, observed platform.Observation) error {
	failure, rejection := classifyFailureDetail(c.Kind, observed.Stdout, observed.Stderr)
	return &InvocationError{
		Cause:        fmt.Errorf("%s invocation failed: %w", c.Kind, cause),
		LastActivity: observed.LastActivity,
		OutputBytes:  len(observed.Output),
		Failure:      failure,
		Rejection:    rejection,
	}
}

// classifyFailure reads only documented provider error envelopes emitted by
// the provider process. In particular, it does not scan arbitrary successful
// event text, which may quote source files, prompts, or test fixtures.
func classifyFailure(kind, stdout, stderr string) FailureClass {
	failure, _ := classifyFailureDetail(kind, stdout, stderr)
	return failure
}

func classifyFailureDetail(kind, stdout, stderr string) (FailureClass, string) {
	for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
		var record struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Error   json.RawMessage `json:"error"`
			IsError bool            `json:"is_error"`
			Result  string          `json:"result"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		var message, rejection string
		switch kind {
		case "codex":
			if record.Type != "turn.failed" {
				continue
			}
			if len(record.Error) == 0 {
				continue
			}
			var detail struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(record.Error, &detail) != nil {
				continue
			}
			message = detail.Code + " " + detail.Message
			rejection = documentedRequestRejection(detail.Code, detail.Message)
		case "claude-code":
			if record.Type != "result" || !record.IsError {
				continue
			}
			message = record.Result
		default:
			continue
		}
		if authenticationMessage(message) {
			return FailureAuthentication, ""
		}
		if rejection != "" {
			return FailureRequestRejected, rejection
		}
	}
	return FailureUnknown, ""
}

// documentedRequestRejection accepts a provider error code only from the
// error object of a failed provider event. Codex may place the API error JSON
// inside error.message, so decode that value as JSON rather than searching it.
// This deliberately cannot classify source text that merely quotes a code.
func documentedRequestRejection(code, message string) string {
	if knownRequestRejection(code) {
		return code
	}
	var nested struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(message), &nested) != nil {
		return ""
	}
	if knownRequestRejection(nested.Code) {
		return nested.Code
	}
	if knownRequestRejection(nested.Error.Code) {
		return nested.Error.Code
	}
	return ""
}

func knownRequestRejection(code string) bool { return code == RejectionInvalidJSONSchema }

// SchemaSHA256 is the stable digest used to scope a provider schema hold. It
// hashes the exact schema sent to both supported provider CLIs.
func SchemaSHA256() string {
	sum := sha256.Sum256([]byte(Schema()))
	return fmt.Sprintf("%x", sum[:])
}

func authenticationMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "http 401") ||
		strings.Contains(message, "401 unauthorized") ||
		strings.Contains(message, "authentication failed") ||
		strings.Contains(message, "not authenticated") ||
		strings.Contains(message, "invalid api key")
}

// resolvedPath evaluates every existing component and reconstructs missing
// suffixes. This catches a scratch symlink/junction that points into source
// before a worker can create cache files there.
func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	missing := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve worker scratch path: %w", err)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("resolve worker scratch path: %w", err)
		}
		missing = append(missing, filepath.Base(abs))
		abs = parent
	}
}

// recoverInProgress returns only a valid structured handoff that was already
// emitted before the process deadline. It never treats arbitrary partial output
// as a successful provider result.
func (c CLI) recoverInProgress(stdout, resultPath, role string) (Result, error) {
	out := stdout
	if c.Kind == "codex" {
		b, err := os.ReadFile(resultPath)
		if err != nil {
			return Result{}, err
		}
		out = string(b)
	} else if c.Kind == "claude-code" {
		var envelope struct {
			Structured json.RawMessage `json:"structured_output"`
			Result     string          `json:"result"`
			IsError    bool            `json:"is_error"`
		}
		if err := json.Unmarshal([]byte(out), &envelope); err != nil || envelope.IsError {
			return Result{}, errors.New("no valid Claude structured handoff")
		}
		if len(envelope.Structured) > 0 {
			out = string(envelope.Structured)
		} else {
			out = envelope.Result
		}
	}
	return Parse(out, role)
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
	for i := range r.Findings {
		f := &r.Findings[i]
		if !roles.Severity(f.Severity) || f.Reason == "" {
			return r, errors.New("malformed finding")
		}
		if f.Relevance == "" {
			f.Relevance = model.FindingUnknown
		}
		if f.Relevance != model.FindingChanged && f.Relevance != model.FindingCausal && f.Relevance != model.FindingBaseline && f.Relevance != model.FindingUnknown {
			return r, errors.New("invalid finding relevance")
		}
		if f.Relevance != model.FindingBaseline && (f.BaselineSHA != "" || f.BaselineEvidence != "") {
			return r, errors.New("baseline evidence requires baseline relevance")
		}
		if f.Relevance == model.FindingBaseline && (f.BaselineSHA == "" || f.BaselineEvidence == "") {
			return r, errors.New("baseline finding requires base revision and evidence")
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
		sort.Strings(required)
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	finding := obj(map[string]any{"severity": map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low", "nit"}}, "category": str, "location": str, "reason": str, "suggested_resolution": str, "relevance": map[string]any{"type": "string", "enum": []string{model.FindingChanged, model.FindingCausal, model.FindingBaseline, model.FindingUnknown}}, "baseline_sha": str, "baseline_evidence": str})
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
