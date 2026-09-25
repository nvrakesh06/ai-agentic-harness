package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
)

const (
	nativeFailureReportEnv = "AIH_FAILURE_REPORT"
	maxNativeFailureReport = 4096
)

var nativeFailureID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

// nativeFailureReport is deliberately small. It identifies a diagnostic, not
// an authority grant: its contents must reproduce from the canonical base
// before a check failure can be routed to a different writer.
type nativeFailureReport struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

func parseNativeFailureReport(file string) (*nativeFailureReport, error) {
	if file == "" {
		return nil, nil
	}
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxNativeFailureReport {
		return nil, errors.New("failure report is not a bounded regular file")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var report nativeFailureReport
	if err = dec.Decode(&report); err != nil {
		return nil, errors.New("failure report must be one strict JSON object")
	}
	var trailing any
	if err = dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("failure report must be one strict JSON object")
	}
	if !nativeFailureID.MatchString(report.ID) || !validNativeFailurePath(report.Path) {
		return nil, errors.New("failure report has an invalid ID or path")
	}
	return &report, nil
}

func validNativeFailurePath(p string) bool {
	if p == "" || strings.Contains(p, `\`) || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "./") || strings.Contains(p, "..") {
		return false
	}
	return safety.Path(p) == nil
}

func nativeFailureReportPath(check config.Check) (string, func(), error) {
	if !check.FailureReport {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp("", "aih-native-failure-")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "report.json"), func() { _ = os.RemoveAll(dir) }, nil
}

func nativeFailureEnvironment(base []string, report string) []string {
	if report == "" {
		return base
	}
	env := append([]string(nil), base...)
	return append(env, nativeFailureReportEnv+"="+report)
}

func sameNativeFailureReport(left, right *nativeFailureReport) bool {
	return left != nil && right != nil && left.ID == right.ID && left.Path == right.Path
}

func completedMatchingBaselineFailure(err error, candidate, baseline *nativeFailureReport) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && sameNativeFailureReport(candidate, baseline)
}

func failureReportTrackedAt(ctxErr error, files []string, report *nativeFailureReport) bool {
	if ctxErr != nil || report == nil {
		return false
	}
	for _, file := range files {
		if file == report.Path {
			return true
		}
	}
	return false
}

func immutableAreaFingerprint(task *model.Task) (string, bool) {
	areas, ok := model.ImmutableAreas(task)
	if !ok || len(areas) == 0 {
		return "", false
	}
	kinds := model.ImmutableAreaKinds(task)
	parts := make([]string, 0, len(areas))
	for _, area := range areas {
		kind := kinds[area]
		if kind != model.AreaFile && kind != model.AreaDirectory {
			return "", false
		}
		parts = append(parts, area+"\x00"+kind)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum), true
}

// routeNativeFailure records a dependency only after the caller has reproduced
// a report at canonical base. It intentionally shares immutable owner lookup,
// cycle prevention, and durable queued guidance with review finding routing.
func (c *Controller) routeNativeFailure(originID string, check config.Check, report *nativeFailureReport, effective config.Effective) (bool, error) {
	if report == nil || !check.FailureReport {
		return false, nil
	}
	routed := false
	err := c.mutate(func(s *model.Snapshot) error {
		routed = applyNativeFailureRoute(s, originID, check, report, effective, c.nowUTC)
		return nil
	})
	return routed, err
}

func applyNativeFailureRoute(s *model.Snapshot, originID string, check config.Check, report *nativeFailureReport, effective config.Effective, now func() time.Time) bool {
	if s == nil || report == nil || !check.FailureReport {
		return false
	}
	origin := s.Tasks[originID]
	if origin == nil || origin.BaseSHA != effective.BaseSHA || origin.HeadSHA == "" {
		return false
	}
	finding := model.Finding{Severity: "high", Category: "native-check", Location: report.Path, Reason: "Canonical-base native check reproduced " + report.ID + ".", Resolution: "Repair the owned failure before the dependent merge train may continue.", Role: "verification"}
	candidate := findingScopeOwner(s.Tasks, origin, finding)
	if candidate.id == "" || candidate.ambiguous || introducesDependencyCycle(s.Tasks, originID, candidate.id) {
		return false
	}
	owner := s.Tasks[candidate.id]
	// Snapshot these identities before the route. This first increment keeps
	// the durable relation in the existing dependency and typed Guidance
	// records; it deliberately does not introduce another snapshot migration
	// while the area-ownership migration stack is still being integrated.
	areas, ok := immutableAreaFingerprint(owner)
	if !ok || !findingInTaskScope(owner, finding) {
		return false
	}
	message := fmt.Sprintf("Canonical-base native check %q reproduced failure %q at %s for dependent task %s (source base %s, head %s, policy %s, owner areas %s). Repair this owned failure before it can merge.", check.Name, report.ID, report.Path, originID, origin.BaseSHA, origin.HeadSHA, effective.Hash, areas)
	sum := sha256.Sum256([]byte(strings.Join([]string{"native-failure", check.Name, report.ID, report.Path, owner.ID, origin.BaseSHA, origin.HeadSHA, effective.Hash, areas}, "\x00")))
	if err := model.QueueRoutedFinding(owner, origin, fmt.Sprintf("native-failure:%x", sum), message); err != nil {
		return false
	}
	if !containsTask(origin.Dependencies, owner.ID) {
		origin.Dependencies = append(origin.Dependencies, owner.ID)
	}
	origin.State = model.SyncRequired
	origin.Blocker = nil
	origin.Updated = now().UTC()
	owner.Findings = appendUniqueFindings(owner.Findings, []model.Finding{finding})
	owner.Decisions = append(owner.Decisions, "Canonical-base native failure routed from "+originID+".")
	return true
}

// trackedReportAtBase makes report paths diagnostic only until the supervisor
// independently verifies that the path is an existing tracked canonical-base
// file. It rejects missing, generated, and untracked paths.
func (c *Controller) trackedReportAtBase(report *nativeFailureReport, base string) bool {
	if report == nil || base == "" {
		return false
	}
	files, err := c.P.Git.Files(c.ctx, base, report.Path)
	return failureReportTrackedAt(err, files, report)
}

func (c *Controller) reproduceAndRouteNativeFailure(id string, failure *checkFailure, effective config.Effective) (bool, error) {
	if failure == nil || failure.report == nil || !failure.check.FailureReport {
		return false, nil
	}
	task := c.Snapshot().Tasks[id]
	if task == nil || task.BaseSHA != effective.BaseSHA || task.HeadSHA == "" || !c.trackedReportAtBase(failure.report, effective.BaseSHA) {
		return false, nil
	}
	base, head, configHash := task.BaseSHA, task.HeadSHA, effective.Hash
	release, err := c.checkPermit(c.ctx, id, failure.check)
	if err != nil {
		return false, err
	}
	defer release()
	dir, err := os.MkdirTemp(c.P.Dir, "native-failure-base-")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = c.P.Git.Detached(c.ctx, dir, effective.BaseSHA); err != nil {
		return false, err
	}
	defer c.P.Git.RemoveWorktree(c.ctx, dir)
	_, baselineErr, baselineReport := runVerificationCheck(c.ctx, failure.check, dir)
	if !completedMatchingBaselineFailure(baselineErr, failure.report, baselineReport) || !c.trackedReportAtBase(baselineReport, base) {
		return false, nil
	}
	// The base check can be slow. Re-read canonical policy and task state
	// before a durable route is persisted so a main/config/source/area change
	// cannot turn old diagnostic output into a dependency on new work.
	refreshed, err := c.effective(c.ctx)
	if err != nil {
		return false, err
	}
	current := c.Snapshot().Tasks[id]
	if refreshed.BaseSHA != base || refreshed.Hash != configHash || current == nil || current.BaseSHA != base || current.HeadSHA != head {
		return false, nil
	}
	return c.routeNativeFailure(id, failure.check, failure.report, refreshed)
}
