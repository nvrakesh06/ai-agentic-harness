package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestParseNativeFailureReportRejectsUntrustedShapes(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(valid, []byte(`{"id":"TS2740","path":"src/studio/server.ts"}`), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := parseNativeFailureReport(valid)
	if err != nil || report == nil || report.ID != "TS2740" {
		t.Fatalf("valid report rejected: report=%#v err=%v", report, err)
	}
	for name, body := range map[string]string{
		"unknown field":  `{"id":"TS2740","path":"src/studio/server.ts","owner":"other"}`,
		"traversal":      `{"id":"TS2740","path":"../secret"}`,
		"backslash":      `{"id":"TS2740","path":"src\\studio\\server.ts"}`,
		"trailing value": `{"id":"TS2740","path":"src/studio/server.ts"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := parseNativeFailureReport(file); err == nil || got != nil {
				t.Fatalf("unsafe report accepted: %#v %v", got, err)
			}
		})
	}
}

func routedFailureSnapshot() (*model.Snapshot, config.Effective, *nativeFailureReport, config.Check) {
	base := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	hash := strings.Repeat("c", 64)
	origin := ownedTask("renderer", model.Verifying, "src/remotion", model.AreaDirectory)
	origin.BaseSHA, origin.HeadSHA = base, head
	origin.ObjectiveID = "objective"
	owner := ownedTask("studio", model.Ready, "src/studio", model.AreaDirectory)
	s := model.NewSnapshot("native-route")
	s.Tasks = map[string]*model.Task{"renderer": origin, "studio": owner}
	return s, config.Effective{BaseSHA: base, Hash: hash}, &nativeFailureReport{ID: "TS2740", Path: "src/studio/server.ts"}, config.Check{Name: "verify", FailureReport: true}
}

func TestNativeFailureRouteRequiresUniqueWritableImmutableOwner(t *testing.T) {
	s, effective, report, check := routedFailureSnapshot()
	if !applyNativeFailureRoute(s, "renderer", check, report, effective, func() time.Time { return time.Unix(1, 0) }) {
		t.Fatal("reproduced native failure was not routed to its unique owner")
	}
	origin, owner := s.Tasks["renderer"], s.Tasks["studio"]
	if origin.State != model.SyncRequired || !containsTask(origin.Dependencies, "studio") || len(model.TaskGuidance(owner)) != 1 {
		t.Fatalf("route did not preserve dependency and durable guidance: origin=%#v owner=%#v", origin, owner)
	}

	for name, mutate := range map[string]func(*model.Snapshot){
		"ambiguous": func(s *model.Snapshot) {
			s.Tasks["other"] = ownedTask("other", model.Ready, "src/studio", model.AreaDirectory)
		},
		"cycle": func(s *model.Snapshot) {
			s.Tasks["studio"].Dependencies = []string{"renderer"}
		},
		"non-writable": func(s *model.Snapshot) {
			s.Tasks["studio"].State = model.Done
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, eff, candidateReport, candidateCheck := routedFailureSnapshot()
			mutate(candidate)
			if applyNativeFailureRoute(candidate, "renderer", candidateCheck, candidateReport, eff, time.Now) {
				t.Fatalf("unsafe %s owner route was accepted", name)
			}
			if len(candidate.Tasks["renderer"].Dependencies) != 0 {
				t.Fatal("rejected route added a dependency")
			}
		})
	}
}

func TestNativeFailureRouteRejectsStaleCanonicalIdentity(t *testing.T) {
	s, effective, report, check := routedFailureSnapshot()
	effective.BaseSHA = strings.Repeat("d", 40)
	if applyNativeFailureRoute(s, "renderer", check, report, effective, time.Now) {
		t.Fatal("route accepted a changed canonical base")
	}
	if s.Tasks["renderer"].State != model.Verifying {
		t.Fatal("stale identity changed source task state")
	}
}

func TestIntegrationClassifiesStructuredNativeCheckFailure(t *testing.T) {
	failure := &checkFailure{name: "verify", check: config.Check{Name: "verify", FailureReport: true}, report: &nativeFailureReport{ID: "TS2740", Path: "src/studio/server.ts"}}
	got, ok := nativeCheckFailure(failure)
	if !ok || got != failure {
		t.Fatalf("merge-train failure did not retain native route evidence: got=%#v ok=%t", got, ok)
	}
	if _, ok := nativeCheckFailure(errors.New("ordinary integration error")); ok {
		t.Fatal("ordinary integration error was treated as a routable native check failure")
	}
}

func TestCompletedMatchingBaselineFailureRejectsTimeoutAndCancellation(t *testing.T) {
	report := &nativeFailureReport{ID: "TS2740", Path: "src/studio/server.ts"}
	if !completedMatchingBaselineFailure(errors.New("exit status 1"), report, report) {
		t.Fatal("ordinary completed baseline failure was not eligible")
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if completedMatchingBaselineFailure(err, report, report) {
			t.Fatalf("non-completed baseline %v was eligible", err)
		}
	}
}
