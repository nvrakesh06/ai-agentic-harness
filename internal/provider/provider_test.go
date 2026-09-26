package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	if os.Getenv("AIH_PROVIDER_HELPER") != "1" {
		return
	}
	if name := os.Getenv("AIH_HELPER_ARGS"); name != "" {
		_ = os.WriteFile(name, []byte(strings.Join(os.Args, "\n")), 0600)
	}
	if name := os.Getenv("AIH_HELPER_SCRATCH_ENV"); name != "" {
		values := []string{}
		for _, key := range []string{"AIH_SCRATCH", "TMP", "TEMP", "TMPDIR", "npm_config_cache", "NPM_CONFIG_CACHE"} {
			values = append(values, key+"="+os.Getenv(key))
		}
		_ = os.WriteFile(name, []byte(strings.Join(values, "\n")), 0600)
	}
	args := strings.Join(os.Args, " ")
	if expected := os.Getenv("AIH_EXPECT_MODEL"); expected != "" {
		actual := ""
		for i, arg := range os.Args {
			if arg == "--model" && i+1 < len(os.Args) {
				actual = os.Args[i+1]
			}
		}
		if actual != expected {
			fmt.Fprintf(os.Stderr, "model = %q, want %q", actual, expected)
			os.Exit(2)
		}
	}
	if strings.Contains(args, "login status") || strings.Contains(args, "auth status") {
		if os.Getenv("AIH_HELPER_MODE") == "unauthenticated" {
			fmt.Println("secret diagnostic must not escape")
			os.Exit(1)
		}
		os.Exit(0)
	}
	if strings.Contains(args, "--help") {
		fmt.Println("--output-schema --output-last-message --sandbox --json-schema --output-format --permission-mode --safe-mode")
		os.Exit(0)
	}
	if os.Getenv("AIH_HELPER_MODE") == "crash" {
		os.Exit(2)
	}
	if os.Getenv("AIH_HELPER_MODE") == "auth-error" {
		if strings.Contains(args, "--output-format") {
			fmt.Fprintln(os.Stderr, `{"type":"result","is_error":true,"result":"HTTP 401 Unauthorized token=abcdefghijklmnopqrstuvwxyz0123456789"}`)
		} else {
			fmt.Fprintln(os.Stderr, `{"type":"turn.failed","error":{"code":"unauthorized","message":"HTTP 401 Unauthorized token=abcdefghijklmnopqrstuvwxyz0123456789"}}`)
		}
		os.Exit(1)
	}
	if os.Getenv("AIH_HELPER_MODE") == "quoted-auth" {
		fmt.Fprintln(os.Stderr, `{"type":"item.completed","item":{"type":"agent_message","text":"repository fixture quotes HTTP 401 Unauthorized"}}`)
		os.Exit(1)
	}
	if os.Getenv("AIH_HELPER_MODE") == "active-timeout" {
		for {
			fmt.Println(`{"type":"command_execution","status":"completed","command":"go test ./internal/provider"}`)
			time.Sleep(10 * time.Millisecond)
		}
	}
	if os.Getenv("AIH_HELPER_MODE") == "timeout" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	r := Result{Schema: 1, Status: "completed", Summary: "fixture result"}
	if os.Getenv("AIH_HELPER_MODE") == "late-in-progress" {
		r = Result{Schema: 1, Status: "in_progress", Summary: "safe checkpoint before timeout"}
	}
	b, _ := json.Marshal(r)
	if os.Getenv("AIH_HELPER_MODE") == "malformed" {
		b = []byte("not-json")
	}
	for i, a := range os.Args {
		if a == "--output-last-message" {
			_ = os.WriteFile(os.Args[i+1], b, 0600)
			if os.Getenv("AIH_HELPER_MODE") == "late-in-progress" {
				time.Sleep(10 * time.Second)
			}
			fmt.Println(`{"type":"turn.completed"}`)
			os.Exit(0)
		}
	}
	envelope, _ := json.Marshal(map[string]any{"structured_output": json.RawMessage(b)})
	fmt.Println(string(envelope))
	if os.Getenv("AIH_HELPER_MODE") == "late-in-progress" {
		time.Sleep(10 * time.Second)
	}
	if os.Getenv("AIH_HELPER_MODE") == "stderr-success" {
		fmt.Fprintln(os.Stderr, "benign provider diagnostic")
	}
	os.Exit(0)
}

func TestAdaptersPassConfiguredModelExactly(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	for _, kind := range []string{"codex", "claude-code"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("AIH_EXPECT_MODEL", "provider-specific-model")
			_, err := (CLI{Kind: kind, Executable: exe}).Run(context.Background(), Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "reviewer", Prompt: "fixture", Model: "provider-specific-model", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestImplementerDisablesCodexAgentsAndRecoversStructuredTimeoutHandoff(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	t.Setenv("AIH_HELPER_MODE", "late-in-progress")
	for _, kind := range []string{"codex", "claude-code"} {
		t.Run(kind, func(t *testing.T) {
			result, err := (CLI{Kind: kind, Executable: exe}).Run(context.Background(), Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "implementer", Prompt: "fixture", Timeout: 500 * time.Millisecond})
			if err != nil || result.Status != "in_progress" || !result.RecoveredDeadlineHandoff {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
		})
	}
}

func TestCodexImplementerInvocationDisablesMultiAgentFeature(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	t.Setenv("AIH_HELPER_MODE", "success")
	t.Setenv("AIH_HELPER_ARGS", argsFile)
	_, err = (CLI{Kind: "codex", Executable: exe}).Run(context.Background(), Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "implementer", Prompt: "fixture", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--disable\nmulti_agent") {
		t.Fatalf("implementer invocation did not disable the multi-agent feature: %s", args)
	}
}

func TestAuthenticationFailureRequiresStructuredProviderErrorRecord(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	for _, kind := range []string{"codex", "claude-code"} {
		t.Run(kind+" error record", func(t *testing.T) {
			t.Setenv("AIH_HELPER_MODE", "auth-error")
			runtime := filepath.Join(t.TempDir(), "run")
			_, err := (CLI{Kind: kind, Executable: exe}).Run(context.Background(), Request{Directory: t.TempDir(), Runtime: runtime, Role: "implementer", Prompt: "fixture", Timeout: time.Second})
			if !IsAuthenticationFailure(err) {
				t.Fatalf("error was not typed as authentication failure: %v", err)
			}
			output, readErr := os.ReadFile(filepath.Join(runtime, "output.log"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(output), "abcdefghijklmnopqrstuvwxyz0123456789") {
				t.Fatalf("provider diagnostic was not redacted: %s", output)
			}
		})
	}
	t.Run("quoted repository content", func(t *testing.T) {
		t.Setenv("AIH_HELPER_MODE", "quoted-auth")
		_, err := (CLI{Kind: "codex", Executable: exe}).Run(context.Background(), Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "implementer", Prompt: "fixture", Timeout: time.Second})
		if IsAuthenticationFailure(err) {
			t.Fatalf("quoted repository content was incorrectly classified: %v", err)
		}
	})
	t.Run("process start failure", func(t *testing.T) {
		_, err := (CLI{Kind: "codex", Executable: filepath.Join(t.TempDir(), "missing-provider")}).Run(context.Background(), Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "implementer", Prompt: "fixture", Timeout: time.Second})
		if err == nil || IsAuthenticationFailure(err) {
			t.Fatalf("process start failure classification = authentication=%t, err=%v", IsAuthenticationFailure(err), err)
		}
	})
}

func TestWorkerScratchIsExternalAndConfiguresTemporaryToolCaches(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "worktree")
	scratch := filepath.Join(filepath.Dir(workspace), "scratch", "task")
	envFile := filepath.Join(t.TempDir(), "scratch-env.txt")
	if err = os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	t.Setenv("AIH_HELPER_SCRATCH_ENV", envFile)
	if _, err = (CLI{Kind: "codex", Executable: exe}).Run(context.Background(), Request{Directory: workspace, Runtime: filepath.Join(t.TempDir(), "run"), Scratch: scratch, Role: "implementer", Prompt: "fixture", Timeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	resolvedScratch, err := resolvedPath(scratch)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"AIH_SCRATCH=" + resolvedScratch,
		"TMP=" + resolvedScratch,
		"TEMP=" + resolvedScratch,
		"TMPDIR=" + resolvedScratch,
		"npm_config_cache=" + filepath.Join(resolvedScratch, "npm-cache"),
		"NPM_CONFIG_CACHE=" + filepath.Join(resolvedScratch, "npm-cache"),
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("scratch environment omitted %q: %s", want, got)
		}
	}
	if _, err = os.Stat(filepath.Join(scratch, "npm-cache")); err != nil {
		t.Fatal(err)
	}
	if _, err = (CLI{Kind: "codex", Executable: exe}).Run(context.Background(), Request{Directory: workspace, Runtime: filepath.Join(t.TempDir(), "invalid-run"), Scratch: filepath.Join(workspace, "scratch"), Role: "implementer", Prompt: "fixture", Timeout: time.Second}); err == nil {
		t.Fatal("scratch inside source worktree was accepted")
	}
	link := filepath.Join(filepath.Dir(workspace), "scratch-link")
	if err = os.Symlink(workspace, link); err == nil {
		if _, err = (CLI{Kind: "codex", Executable: exe}).Run(context.Background(), Request{Directory: workspace, Runtime: filepath.Join(t.TempDir(), "junction-run"), Scratch: link, Role: "implementer", Prompt: "fixture", Timeout: time.Second}); err == nil {
			t.Fatal("scratch symlink into source worktree was accepted")
		}
	} else {
		t.Logf("symlink fixture unavailable on this machine: %v", err)
	}
}
func TestAdaptersLaunchAndFailures(t *testing.T) {
	// A tiny Go launcher runs this test executable as a mock CLI; the same CLI
	// argument/result paths are exercised on Windows and Unix.
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	for _, kind := range []string{"codex", "claude-code"} {
		t.Run(kind, func(t *testing.T) {
			c := CLI{Kind: kind, Executable: exe}
			if e := c.Validate(context.Background()); e != nil {
				t.Fatal(e)
			}
			for _, mode := range []string{"success", "stderr-success", "malformed", "crash", "timeout", "active-timeout"} {
				t.Run(mode, func(t *testing.T) {
					t.Setenv("AIH_HELPER_MODE", mode)
					r := Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "reviewer", Prompt: "fixture", Timeout: 2 * time.Second}
					if mode == "timeout" || mode == "active-timeout" {
						r.Timeout = 100 * time.Millisecond
					}
					if mode == "active-timeout" {
						r.Timeout = time.Second
					}
					result, e := c.Run(context.Background(), r)
					if mode == "success" || mode == "stderr-success" {
						if e != nil || result.Summary != "fixture result" {
							t.Fatal(result, e)
						}
					} else if e == nil {
						t.Fatal("failure not detected", mode)
					} else if mode == "active-timeout" {
						var invocation *InvocationError
						if !errors.As(e, &invocation) || invocation.OutputBytes == 0 || invocation.LastActivity.IsZero() {
							t.Fatalf("active timeout lost output evidence: %#v %v", invocation, e)
						}
					}
				})
			}
		})
	}
}
func TestStructuredResults(t *testing.T) {
	r := Result{Schema: 1, Status: "completed", Summary: "done"}
	b, _ := json.Marshal(r)
	if _, e := Parse(string(b), "implementer"); e != nil {
		t.Fatal(e)
	}
	verificationOnly := Result{Schema: 1, Status: "blocked", Summary: "implementation complete; native verification unavailable"}
	b, _ = json.Marshal(verificationOnly)
	if _, e := Parse(string(b), "implementer"); e != nil {
		t.Fatal("verification-only implementer result rejected", e)
	}
	if _, e := Parse(string(b), "qa"); e == nil {
		t.Fatal("questionless advisory blocker accepted")
	}
	for _, input := range []string{`{}`, `{"schema_version":9,"status":"completed","summary":"x"}`, `oops`} {
		if _, e := Parse(input, "implementer"); e == nil {
			t.Fatal("malformed output accepted")
		}
	}
}

func TestFindingRelevanceRequiresBaselineProof(t *testing.T) {
	base := strings.Repeat("a", 40)
	valid := `{"schema_version":1,"status":"completed","summary":"done","question":"","changed_areas":[],"tests_run":[],"remaining_risks":[],"findings":[{"severity":"medium","category":"reliability","location":"internal/studio/base.go:12","reason":"Same failure reproduces at base.","suggested_resolution":"Route to the owner.","relevance":"baseline","baseline_sha":"` + base + `","baseline_evidence":"go test ./internal/studio at base fails identically"}],"plan":[]}`
	result, err := Parse(valid, "reviewer")
	if err != nil || result.Findings[0].Relevance != model.FindingBaseline {
		t.Fatalf("valid baseline finding = %#v, %v", result, err)
	}
	emptyNonBaseline := strings.Replace(strings.Replace(strings.Replace(valid, `"relevance":"baseline"`, `"relevance":"causal"`, 1), `"baseline_sha":"`+base+`"`, `"baseline_sha":""`, 1), `"baseline_evidence":"go test ./internal/studio at base fails identically"`, `"baseline_evidence":""`, 1)
	if _, err := Parse(emptyNonBaseline, "reviewer"); err != nil {
		t.Fatalf("non-baseline finding with required empty proof fields was rejected: %v", err)
	}
	for _, input := range []string{
		strings.Replace(valid, `"baseline_evidence":"go test ./internal/studio at base fails identically"`, `"baseline_evidence":""`, 1),
		strings.Replace(valid, `"relevance":"baseline"`, `"relevance":"not-proven"`, 1),
		strings.Replace(valid, `"relevance":"baseline"`, `"relevance":"causal"`, 1),
	} {
		if _, err := Parse(input, "reviewer"); err == nil {
			t.Fatalf("invalid relevance proof accepted: %s", input)
		}
	}
}

func TestSchemaObjectsDeclareExactlyTheirProperties(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(Schema()), &schema); err != nil {
		t.Fatal(err)
	}
	assertStrictSchemaObjects(t, schema, "$")
}

func assertStrictSchemaObjects(t *testing.T, node any, path string) {
	t.Helper()
	object, ok := node.(map[string]any)
	if !ok {
		return
	}
	if object["type"] == "object" {
		properties, ok := object["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s object schema has no properties", path)
		}
		if additional, ok := object["additionalProperties"].(bool); !ok || additional {
			t.Fatalf("%s object schema is not strict: %#v", path, object["additionalProperties"])
		}
		required, ok := object["required"].([]any)
		if !ok {
			t.Fatalf("%s object schema has no required keys", path)
		}
		seen := make(map[string]bool, len(required))
		for _, value := range required {
			key, ok := value.(string)
			if !ok || seen[key] {
				t.Fatalf("%s has invalid required key %#v", path, value)
			}
			seen[key] = true
		}
		if len(seen) != len(properties) {
			t.Fatalf("%s required keys %v do not match properties %v", path, required, properties)
		}
		for key := range properties {
			if !seen[key] {
				t.Fatalf("%s property %q is not required", path, key)
			}
		}
		for key, child := range properties {
			assertStrictSchemaObjects(t, child, path+".properties."+key)
		}
	}
	if items, ok := object["items"]; ok {
		assertStrictSchemaObjects(t, items, path+".items")
	}
}

func TestPlanRiskSchemaMatchesValidation(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(Schema()), &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	plan := properties["plan"].(map[string]any)
	task := plan["items"].(map[string]any)
	taskProperties := task["properties"].(map[string]any)
	risk := taskProperties["risk"].(map[string]any)
	values := risk["enum"].([]any)
	want := []string{"low", "medium", "high"}
	if len(values) != len(want) {
		t.Fatalf("risk enum = %v, want %v", values, want)
	}
	for i, value := range values {
		if value != want[i] {
			t.Fatalf("risk enum = %v, want %v", values, want)
		}
	}
}

func TestPlanReadinessSchemaMatchesValidation(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(Schema()), &schema); err != nil {
		t.Fatal(err)
	}
	plan := schema["properties"].(map[string]any)["plan"].(map[string]any)
	if _, constrained := plan["minItems"]; constrained {
		t.Fatal("shared schema requires plans from non-orchestrator or blocked results")
	}
	if plan["maxItems"] != float64(50) {
		t.Fatalf("plan maximum = %v", plan["maxItems"])
	}
	properties := plan["items"].(map[string]any)["properties"].(map[string]any)
	if properties["key"].(map[string]any)["pattern"] != model.PlanKeyPattern {
		t.Fatal("plan key pattern drifted from model validation")
	}
	for _, name := range []string{"title", "objective"} {
		if properties[name].(map[string]any)["minLength"] != float64(1) {
			t.Fatalf("%s permits an empty value", name)
		}
	}
	for _, name := range []string{"acceptance", "areas", "conflict_domains"} {
		if properties[name].(map[string]any)["minItems"] != float64(1) {
			t.Fatalf("%s permits an empty list", name)
		}
	}
}

func TestProviderPathAndAuthentication(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("AIH_PROVIDER_HELPER", "1")
	t.Setenv("CODEX_BINARY", exe)
	t.Setenv("CLAUDE_BINARY", exe)
	for _, kind := range []string{"codex", "claude-code"} {
		if New(kind).(CLI).Executable != exe {
			t.Fatal("binary override ignored")
		}
		t.Setenv("AIH_HELPER_MODE", "success")
		if err := CheckAuthentication(context.Background(), kind); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AIH_HELPER_MODE", "unauthenticated")
		if err := CheckAuthentication(context.Background(), kind); err == nil || strings.Contains(err.Error(), "secret diagnostic") {
			t.Fatal("authentication failure missing or sensitive output leaked", err)
		}
	}
}
