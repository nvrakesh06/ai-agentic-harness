package provider

import (
	"context"
	"encoding/json"
	"fmt"
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
	args := strings.Join(os.Args, " ")
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
	if os.Getenv("AIH_HELPER_MODE") == "timeout" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	r := Result{Schema: 1, Status: "completed", Summary: "fixture result"}
	b, _ := json.Marshal(r)
	if os.Getenv("AIH_HELPER_MODE") == "malformed" {
		b = []byte("not-json")
	}
	for i, a := range os.Args {
		if a == "--output-last-message" {
			_ = os.WriteFile(os.Args[i+1], b, 0600)
			fmt.Println(`{"type":"turn.completed"}`)
			os.Exit(0)
		}
	}
	envelope, _ := json.Marshal(map[string]any{"structured_output": json.RawMessage(b)})
	fmt.Println(string(envelope))
	os.Exit(0)
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
			for _, mode := range []string{"success", "malformed", "crash", "timeout"} {
				t.Run(mode, func(t *testing.T) {
					t.Setenv("AIH_HELPER_MODE", mode)
					r := Request{Directory: t.TempDir(), Runtime: filepath.Join(t.TempDir(), "run"), Role: "reviewer", Prompt: "fixture", Timeout: 2 * time.Second}
					if mode == "timeout" {
						r.Timeout = 100 * time.Millisecond
					}
					result, e := c.Run(context.Background(), r)
					if mode == "success" {
						if e != nil || result.Summary != "fixture result" {
							t.Fatal(result, e)
						}
					} else if e == nil {
						t.Fatal("failure not detected", mode)
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
	for _, input := range []string{`{}`, `{"schema_version":9,"status":"completed","summary":"x"}`, `{"schema_version":1,"status":"blocked","summary":"x","question":""}`, `oops`} {
		if _, e := Parse(input, "implementer"); e == nil {
			t.Fatal("malformed output accepted")
		}
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
