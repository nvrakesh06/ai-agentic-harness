// Package model defines portable state. It must never contain machine paths,
// process identifiers, credentials, or provider conversation history.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const Version = "1.0.0"
const StateSchema = 4
const RulesVersion = 1
const RoleSchema = 1
const CapacityTransitionLimit = 20
const MaxTaskGuidance = 8
const MaxGuidanceBytes = 1600
const guidancePrefix = "AIH_GUIDANCE_V1:"

type State string

const (
	Planned      State = "PLANNED"
	Ready        State = "READY"
	Running      State = "RUNNING"
	Implemented  State = "IMPLEMENTED"
	SyncRequired State = "SYNC_REQUIRED"
	Verifying    State = "VERIFYING"
	Review       State = "REVIEW"
	Fix          State = "FIX"
	MergeReady   State = "MERGE_READY"
	MergeTrain   State = "MERGE_TRAIN"
	PostVerify   State = "POST_VERIFY"
	Done         State = "DONE"
	Blocked      State = "BLOCKED_HUMAN"
)

var edges = map[State][]State{
	Planned: {Ready, Blocked}, Ready: {Running, Blocked}, Running: {Implemented, Ready, Fix, Blocked},
	Implemented: {SyncRequired, Blocked}, SyncRequired: {Verifying, Fix, Blocked},
	Verifying: {Review, Fix, SyncRequired, Blocked}, Review: {Fix, MergeReady, SyncRequired, Blocked},
	Fix: {Running, Blocked}, MergeReady: {MergeTrain, SyncRequired, Blocked},
	MergeTrain: {PostVerify, SyncRequired, Fix, Blocked}, PostVerify: {Done, Blocked},
	Blocked: {Ready, Fix, SyncRequired, PostVerify, Planned},
}

type Task struct {
	ID               string         `json:"id"`
	ObjectiveID      string         `json:"objective_id"`
	Issue            int            `json:"issue"`
	PR               int            `json:"pr,omitempty"`
	Title            string         `json:"title"`
	Objective        string         `json:"objective"`
	Acceptance       []string       `json:"acceptance"`
	Dependencies     []string       `json:"dependencies"`
	Areas            []string       `json:"areas"`
	Domains          []string       `json:"conflict_domains"`
	Risk             string         `json:"risk"`
	UI               bool           `json:"ui"`
	Security         bool           `json:"security"`
	Roles            []string       `json:"roles"`
	State            State          `json:"state"`
	Branch           string         `json:"branch"`
	BaseSHA          string         `json:"base_sha,omitempty"`
	HeadSHA          string         `json:"head_sha,omitempty"`
	MergeSHA         string         `json:"merge_sha,omitempty"`
	PostVerifySHA    string         `json:"post_verify_sha,omitempty"`
	RecoveryRequired bool           `json:"recovery_required,omitempty"`
	SyncBase         string         `json:"conflict_base,omitempty"`
	Attempts         int            `json:"attempts"`
	Rotations        int            `json:"checkpoint_rotations"`
	FixCycles        map[string]int `json:"fix_cycles"`
	AdvisorUsed      bool           `json:"advisor_used"`
	RunID            string         `json:"run_id,omitempty"`
	Preflight        *Preflight     `json:"preflight,omitempty"`
	Findings         []Finding      `json:"findings,omitempty"`
	Summary          string         `json:"implementation_summary,omitempty"`
	ReportedTests    []string       `json:"reported_tests,omitempty"`
	Risks            []string       `json:"remaining_risks,omitempty"`
	Decisions        []string       `json:"decisions,omitempty"`
	Blocker          *Blocker       `json:"blocker,omitempty"`
	Verification     *Verification  `json:"verification_retry_guard,omitempty"`
	Evidence         *Evidence      `json:"evidence,omitempty"`
	Updated          time.Time      `json:"updated"`
}

// Preflight is portable so completed reader guidance survives a controller restart.
// Ready means the same source and policy may proceed to writer admission.
type Preflight struct {
	Phase       string   `json:"phase"`
	BaseSHA     string   `json:"base_sha"`
	HeadSHA     string   `json:"head_sha,omitempty"`
	Config      string   `json:"config"`
	Rules       string   `json:"rules"`
	Scope       string   `json:"scope_fingerprint,omitempty"`
	ReuseCount  int      `json:"reuse_count,omitempty"`
	ReuseReason string   `json:"reuse_reason,omitempty"`
	Completed   []string `json:"completed,omitempty"`
}

// Guidance is encoded in the existing durable Decisions field so a correction
// survives attach without introducing a competing task-state schema while the
// capacity-state migration is in flight. The command ID makes delivery auditable.
type Guidance struct {
	CommandID string `json:"command_id"`
	SourceID  string `json:"source_task"`
	SourceSHA string `json:"source_head"`
	Text      string `json:"text"`
}

func TaskGuidance(t *Task) []Guidance {
	var out []Guidance
	for _, decision := range t.Decisions {
		if !strings.HasPrefix(decision, guidancePrefix) {
			continue
		}
		var item Guidance
		if json.Unmarshal([]byte(strings.TrimPrefix(decision, guidancePrefix)), &item) == nil {
			out = append(out, item)
		}
	}
	return out
}

func QueueGuidance(target, source *Task, commandID, message string) error {
	if target == nil || source == nil || target.ID == source.ID || target.ObjectiveID == "" || target.ObjectiveID != source.ObjectiveID {
		return errors.New("guidance requires distinct tasks in the same objective")
	}
	if target.State != Ready && target.State != Running && target.State != Fix {
		return errors.New("guidance target must be READY, RUNNING, or FIX")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(source.HeadSHA) {
		return errors.New("guidance source needs a durable code checkpoint")
	}
	message = strings.TrimSpace(message)
	if message == "" || len(message) > MaxGuidanceBytes || !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') {
		return errors.New("guidance must contain 1..1600 UTF-8 bytes without NUL")
	}
	if len(TaskGuidance(target)) >= MaxTaskGuidance {
		return errors.New("task guidance limit reached")
	}
	encoded, err := json.Marshal(Guidance{CommandID: commandID, SourceID: source.ID, SourceSHA: source.HeadSHA, Text: message})
	if err != nil {
		return err
	}
	target.Decisions = append(target.Decisions, guidancePrefix+string(encoded))
	return nil
}

type Verification struct {
	Environment       string `json:"environment"`
	SourceEnvironment string `json:"source_environment,omitempty"`
	HeadSHA           string `json:"head_sha"`
	Fingerprint       string `json:"fingerprint"`
	Attempts          int    `json:"attempts"`
	NativeOnly        bool   `json:"native_only"`
}
type Blocker struct {
	Question       string `json:"question"`
	Reason         string `json:"reason"`
	Impact         string `json:"impact"`
	Recommendation string `json:"recommendation,omitempty"`
	Resume         State  `json:"resume_state"`
}
type Finding struct {
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Location   string `json:"location"`
	Reason     string `json:"reason"`
	Resolution string `json:"suggested_resolution"`
	Role       string `json:"role,omitempty"`
}
type Evidence struct {
	Base               string            `json:"base"`
	Head               string            `json:"head"`
	Config             string            `json:"config"`
	Rules              string            `json:"rules"`
	Checks             []string          `json:"checks"`
	Visual             *VisualEvidence   `json:"visual,omitempty"`
	Reviews            map[string]string `json:"reviews"`
	ReviewRoster       []string          `json:"review_roster,omitempty"`
	ReviewRosterReason string            `json:"review_roster_reason,omitempty"`
	IntegrationSHA     string            `json:"integration_sha,omitempty"`
	IntegrationOwner   string            `json:"integration_owner,omitempty"`
	At                 time.Time         `json:"at"`
}

// VisualEvidence references supervisor-owned local capture artifacts. Image
// bytes and diagnostics remain outside the portable state snapshot.
type VisualEvidence struct {
	Head           string           `json:"head"`
	Config         string           `json:"config"`
	Manifest       string           `json:"manifest"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Artifacts      []VisualArtifact `json:"artifacts"`
	Summary        string           `json:"summary"`
}

type VisualArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Objective struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Issue    int    `json:"issue"`
	Planned  bool   `json:"planned"`
	Attempts int    `json:"attempts"`
	Blocker  string `json:"blocker,omitempty"`
}
type Lease struct {
	Machine   string    `json:"machine_id"`
	Owner     string    `json:"owner"`
	Epoch     uint64    `json:"lease_epoch"`
	Heartbeat time.Time `json:"last_heartbeat"`
	Expires   time.Time `json:"expires_at"`
}
type Run struct {
	ID             string    `json:"id"`
	Task           string    `json:"task"`
	Role           string    `json:"role"`
	Provider       string    `json:"provider"`
	Capability     string    `json:"capability"`
	EffectiveModel string    `json:"effective_model"`
	Version        string    `json:"version"`
	RulesHash      string    `json:"rules_hash"`
	Started        time.Time `json:"started"`
	DurationMS     int64     `json:"duration_ms"`
	Outcome        string    `json:"outcome"`
	Epoch          uint64    `json:"epoch"`
}
type CapacityTransition struct {
	At            time.Time `json:"at"`
	Kind          string    `json:"kind"`
	ActiveWriters int       `json:"active_writers"`
	TargetWriters int       `json:"target_active_writers"`
	ReasonCode    string    `json:"reason_code,omitempty"`
	Objective     string    `json:"objective,omitempty"`
}
type VerificationCheck struct {
	Task      string    `json:"task"`
	Check     string    `json:"check"`
	Class     string    `json:"class"`
	Phase     string    `json:"phase"`
	QueuedAt  time.Time `json:"queued_at"`
	StartedAt time.Time `json:"started_at,omitempty"`
}
type Capacity struct {
	ActiveWriters      int                  `json:"active_writers"`
	ActivePreflights   int                  `json:"active_preflights,omitempty"`
	TargetWriters      int                  `json:"target_active_writers"`
	MaxWriters         int                  `json:"max_parallel_writers"`
	ActiveReaders      int                  `json:"active_readers"`
	MaxReaders         int                  `json:"max_parallel_readers"`
	MaxHeavyChecks     int                  `json:"max_heavy_checks,omitempty"`
	MaxLightChecks     int                  `json:"max_light_checks,omitempty"`
	Verification       []VerificationCheck  `json:"verification,omitempty"`
	GraceSeconds       int                  `json:"underutilization_grace_seconds"`
	BacklogSource      string               `json:"backlog_source"`
	BacklogCursor      int                  `json:"backlog_cursor"`
	State              string               `json:"state"`
	ReasonCode         string               `json:"underutilization_reason_code,omitempty"`
	Reason             string               `json:"underutilization_reason,omitempty"`
	NextSafeWork       string               `json:"next_safe_work,omitempty"`
	LastDispatch       string               `json:"last_backfill_dispatch,omitempty"`
	UnderutilizedSince time.Time            `json:"underutilized_since,omitempty"`
	Transitions        []CapacityTransition `json:"transitions,omitempty"`
}
type Snapshot struct {
	Schema             int                   `json:"state_schema"`
	CreatedBy          string                `json:"created_by_version"`
	Project            string                `json:"project"`
	Revision           uint64                `json:"revision"`
	Controller         Lease                 `json:"controller"`
	Objectives         map[string]*Objective `json:"objectives"`
	Backlog            []string              `json:"authorized_objective_backlog"`
	Capacity           Capacity              `json:"capacity"`
	Tasks              map[string]*Task      `json:"tasks"`
	Runs               []Run                 `json:"runs,omitempty"`
	Applied            map[string]bool       `json:"applied_commands"`
	Improvements       []string              `json:"improvement_candidates,omitempty"`
	IntegrationBlocked string                `json:"integration_blocked,omitempty"`
}

func NewSnapshot(project string) *Snapshot {
	return &Snapshot{Schema: StateSchema, CreatedBy: Version, Project: project,
		Objectives: map[string]*Objective{}, Backlog: []string{}, Tasks: map[string]*Task{}, Applied: map[string]bool{}}
}
func ID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Clone(s *Snapshot) *Snapshot {
	b, _ := json.Marshal(s)
	var out Snapshot
	_ = json.Unmarshal(b, &out)
	return &out
}
func Decode(b []byte) (*Snapshot, bool, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, false, err
	}
	if s.Schema > StateSchema || s.Schema < 0 {
		return nil, false, fmt.Errorf("unsupported remote state schema %d", s.Schema)
	}
	migrated := s.Schema < StateSchema
	if s.Schema == 0 {
		if s.CreatedBy == "" {
			s.CreatedBy = Version
		}
	}
	if s.Schema <= 2 {
		// Earlier runtimes did not own verification resources. Ignore any
		// unexpected fields and reconstruct limits from canonical policy on attach.
		s.Capacity.MaxHeavyChecks = 0
		s.Capacity.MaxLightChecks = 0
		s.Capacity.Verification = nil
	}
	if migrated {
		s.Schema = StateSchema
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{8,80}$`).MatchString(s.Project) {
		return nil, false, errors.New("remote state has no project identity")
	}
	if s.Tasks == nil {
		s.Tasks = map[string]*Task{}
	}
	if s.Objectives == nil {
		s.Objectives = map[string]*Objective{}
	}
	if s.Backlog == nil {
		for id := range s.Objectives {
			s.Backlog = append(s.Backlog, id)
		}
		sort.Strings(s.Backlog)
	}
	if s.Applied == nil {
		s.Applied = map[string]bool{}
	}
	for id, t := range s.Tasks {
		if t == nil || t.ID != id || !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(id) {
			return nil, false, errors.New("invalid task identity")
		}
		if t.Branch != "" && !regexp.MustCompile(`^aih/[a-zA-Z0-9_-]+$`).MatchString(t.Branch) {
			return nil, false, errors.New("unsafe task branch")
		}
		for _, sha := range []string{t.HeadSHA, t.BaseSHA, t.MergeSHA, t.SyncBase, t.PostVerifySHA} {
			if sha != "" && !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(sha) {
				return nil, false, errors.New("invalid task revision")
			}
		}
		if t.State == Blocked && (t.Blocker == nil || t.Blocker.Question == "") {
			return nil, false, errors.New("blocked task has no question")
		}
		if v := t.Verification; v != nil {
			if v.Environment == "" || v.Attempts < 0 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.Fingerprint) {
				return nil, false, errors.New("invalid verification retry guard")
			}
			if v.HeadSHA != "" && !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(v.HeadSHA) {
				return nil, false, errors.New("invalid verification retry revision")
			}
		}
		if p := t.Preflight; p != nil {
			if p.Phase != "queued" && p.Phase != "waiting" && p.Phase != "running" && p.Phase != "ready" && p.Phase != "writing" {
				return nil, false, errors.New("invalid preflight phase")
			}
			if !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(p.BaseSHA) ||
				(p.HeadSHA != "" && !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(p.HeadSHA)) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.Config) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.Rules) {
				return nil, false, errors.New("incomplete preflight identity")
			}
			if (p.Scope != "" && !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(p.Scope)) || p.ReuseCount < 0 {
				return nil, false, errors.New("invalid preflight reuse identity")
			}
		}
		if t.Evidence != nil && t.Evidence.Visual != nil {
			v := t.Evidence.Visual
			if v.Head != t.Evidence.Head || v.Config != t.Evidence.Config ||
				!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(v.Head) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.Config) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.ManifestSHA256) ||
				len(v.Summary) > 1000 || len(v.Artifacts) < 1 || len(v.Artifacts) > 8 ||
				v.Manifest != "visual-evidence/"+id+"/"+v.Head+"-"+v.Config[:16]+"/manifest.json" {
				return nil, false, errors.New("invalid visual evidence reference")
			}
			for _, artifact := range v.Artifacts {
				if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}\.(png|jpg|jpeg|txt|json)$`).MatchString(artifact.Path) || strings.Contains(artifact.Path, "..") || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(artifact.SHA256) {
					return nil, false, errors.New("invalid visual artifact reference")
				}
			}
		}
		if _, ok := edges[t.State]; !ok && t.State != Done {
			return nil, false, fmt.Errorf("unknown task state %q", t.State)
		}
		if t.FixCycles == nil {
			t.FixCycles = map[string]int{}
		}
	}
	for id, o := range s.Objectives {
		if o == nil || o.ID != id || !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(id) {
			return nil, false, errors.New("invalid objective identity")
		}
	}
	seenBacklog := map[string]bool{}
	for _, id := range s.Backlog {
		if s.Objectives[id] == nil || seenBacklog[id] {
			return nil, false, errors.New("invalid authorized objective backlog")
		}
		seenBacklog[id] = true
	}
	if s.Capacity.BacklogCursor < 0 || s.Capacity.BacklogCursor > len(s.Backlog) {
		return nil, false, errors.New("invalid backlog cursor")
	}
	if s.Capacity.TargetWriters != 0 {
		if s.Capacity.TargetWriters < 1 || s.Capacity.MaxWriters < s.Capacity.TargetWriters || s.Capacity.MaxReaders < 1 || s.Capacity.GraceSeconds < 0 || s.Capacity.BacklogSource != "queued_objectives" {
			return nil, false, errors.New("invalid capacity policy")
		}
	}
	if (s.Capacity.MaxHeavyChecks != 0 && (s.Capacity.MaxHeavyChecks < 1 || s.Capacity.MaxHeavyChecks > 8)) || (s.Capacity.MaxLightChecks != 0 && (s.Capacity.MaxLightChecks < 1 || s.Capacity.MaxLightChecks > 8)) {
		return nil, false, errors.New("invalid check capacity policy")
	}
	if s.Capacity.ReasonCode != "" && !regexp.MustCompile(`^[a-z_]+$`).MatchString(s.Capacity.ReasonCode) {
		return nil, false, errors.New("invalid capacity reason code")
	}
	if len(s.Capacity.Transitions) > CapacityTransitionLimit {
		return nil, false, errors.New("capacity transition history exceeds limit")
	}
	seenChecks := map[string]bool{}
	for _, check := range s.Capacity.Verification {
		if s.Tasks[check.Task] == nil || check.Check == "" || (check.Class != "heavy" && check.Class != "light") || (check.Phase != "queued" && check.Phase != "running") || check.QueuedAt.IsZero() || (check.Phase == "running" && check.StartedAt.IsZero()) {
			return nil, false, errors.New("invalid verification capacity record")
		}
		key := check.Task + "\x00" + check.Check
		if seenChecks[key] {
			return nil, false, errors.New("duplicate verification capacity record")
		}
		seenChecks[key] = true
	}
	for _, transition := range s.Capacity.Transitions {
		if transition.At.IsZero() || (transition.Kind != "capacity_underutilized" && transition.Kind != "capacity_backfill_selected" && transition.Kind != "capacity_backfill_suppressed") {
			return nil, false, errors.New("invalid capacity transition")
		}
		if transition.ReasonCode != "" && !regexp.MustCompile(`^[a-z_]+$`).MatchString(transition.ReasonCode) {
			return nil, false, errors.New("invalid capacity transition reason")
		}
		if transition.Objective != "" && s.Objectives[transition.Objective] == nil {
			return nil, false, errors.New("capacity transition references unknown objective")
		}
	}
	for _, t := range s.Tasks {
		for _, dep := range t.Dependencies {
			if s.Tasks[dep] == nil {
				return nil, false, errors.New("missing task dependency")
			}
		}
	}
	return &s, migrated, nil
}
func Transition(t *Task, to State) error {
	for _, s := range edges[t.State] {
		if s == to {
			t.State = to
			t.Updated = time.Now().UTC()
			return nil
		}
	}
	return fmt.Errorf("invalid task transition %s -> %s", t.State, to)
}
func Block(t *Task, question, reason string, resume State) {
	t.Blocker = &Blocker{Question: question, Reason: reason, Impact: "Only this task and its dependants wait.", Resume: resume}
	t.State = Blocked
	t.Updated = time.Now().UTC()
}
func Answer(t *Task, answer string) error {
	if t.State != Blocked || t.Blocker == nil {
		return errors.New("task has no human blocker")
	}
	if strings.TrimSpace(answer) == "" {
		return errors.New("answer cannot be empty")
	}
	next := t.Blocker.Resume
	if err := Transition(t, next); err != nil {
		return err
	}
	t.Decisions = append(t.Decisions, answer)
	if t.Rotations >= 24 {
		t.Decisions = append(t.Decisions, "Human authorized another bounded checkpoint rotation window.")
		t.Rotations = 0
	}
	t.Blocker = nil
	return nil
}
func Ordered(s *Snapshot) []*Task {
	out := make([]*Task, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func Runnable(s *Snapshot, active map[string]bool, limit int) []*Task {
	return RunnableWhere(s, active, limit, func(*Task) bool { return true })
}
func RunnableWhere(s *Snapshot, active map[string]bool, limit int, eligible func(*Task) bool) []*Task {
	domains := map[string]bool{}
	count := 0
	for id := range active {
		if t := s.Tasks[id]; t != nil {
			count++
			for _, d := range t.Domains {
				domains[d] = true
			}
		}
	}
	var result []*Task
	for _, t := range Ordered(s) {
		if count >= limit {
			break
		}
		if active[t.ID] || (t.State != Ready && t.State != Fix) || !eligible(t) {
			continue
		}
		ok := true
		for _, dep := range t.Dependencies {
			if s.Tasks[dep] == nil || s.Tasks[dep].State != Done {
				ok = false
			}
		}
		for _, d := range t.Domains {
			if domains[d] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		result = append(result, t)
		count++
		for _, d := range t.Domains {
			domains[d] = true
		}
	}
	return result
}

type PlanTask struct {
	Key          string   `json:"key"`
	Title        string   `json:"title"`
	Objective    string   `json:"objective"`
	Acceptance   []string `json:"acceptance"`
	Dependencies []string `json:"dependencies"`
	Areas        []string `json:"areas"`
	Domains      []string `json:"conflict_domains"`
	Risk         string   `json:"risk"`
	UI           bool     `json:"ui"`
	Security     bool     `json:"security"`
	Roles        []string `json:"roles"`
}

const (
	PlanKeyPattern = `^[a-z][a-z0-9_-]{0,31}$`
	MinPlanTasks   = 1
	MaxPlanTasks   = 50
)

func ValidatePlan(plan []PlanTask) error {
	if len(plan) < MinPlanTasks || len(plan) > MaxPlanTasks {
		return errors.New("plan must contain 1..50 tasks")
	}
	byKey := map[string]PlanTask{}
	for _, t := range plan {
		if !regexp.MustCompile(PlanKeyPattern).MatchString(t.Key) || t.Title == "" || t.Objective == "" || len(t.Acceptance) == 0 || len(t.Areas) == 0 || len(t.Domains) == 0 {
			return errors.New("task lacks readiness fields or has an invalid key")
		}
		if _, ok := byKey[t.Key]; ok {
			return errors.New("duplicate task key")
		}
		if t.Risk != "low" && t.Risk != "medium" && t.Risk != "high" {
			return errors.New("invalid risk")
		}
		byKey[t.Key] = t
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(k string) error {
		if done[k] {
			return nil
		}
		t, ok := byKey[k]
		if !ok {
			return fmt.Errorf("missing dependency %s", k)
		}
		if visiting[k] {
			return errors.New("cyclic task dependencies")
		}
		visiting[k] = true
		for _, d := range t.Dependencies {
			if err := visit(d); err != nil {
				return err
			}
		}
		visiting[k] = false
		done[k] = true
		return nil
	}
	for k := range byKey {
		if err := visit(k); err != nil {
			return err
		}
	}
	return nil
}
