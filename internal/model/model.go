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
const StateSchema = 7
const RulesVersion = 1
const RoleSchema = 1
const CapacityTransitionLimit = 20
const MaxTaskGuidance = 8
const MaxGuidanceBytes = 1600

// MaxVisualEvidenceArtifacts includes up to eight screenshots and one shared diagnostic log.
const MaxVisualEvidenceArtifacts = 9
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
	ID           string   `json:"id"`
	ObjectiveID  string   `json:"objective_id"`
	Issue        int      `json:"issue"`
	PR           int      `json:"pr,omitempty"`
	Title        string   `json:"title"`
	Objective    string   `json:"objective"`
	Acceptance   []string `json:"acceptance"`
	Dependencies []string `json:"dependencies"`
	Areas        []string `json:"areas"`
	// AssignedAreas is the immutable plan-time ownership boundary. Areas is
	// retained as the human-readable plan scope and may be widened by legacy
	// runtimes, so routing must eventually use AssignedAreas instead.
	AssignedAreas []string `json:"assigned_areas,omitempty"`
	// AssignedAreaKinds captures each AssignedAreas entry as it existed in the
	// base tree. Unknown is an explicit, fail-closed classification.
	AssignedAreaKinds map[string]string           `json:"assigned_area_kinds,omitempty"`
	Domains           []string                    `json:"conflict_domains"`
	Risk              string                      `json:"risk"`
	UI                bool                        `json:"ui"`
	Security          bool                        `json:"security"`
	Roles             []string                    `json:"roles"`
	State             State                       `json:"state"`
	Branch            string                      `json:"branch"`
	BaseSHA           string                      `json:"base_sha,omitempty"`
	HeadSHA           string                      `json:"head_sha,omitempty"`
	MergeSHA          string                      `json:"merge_sha,omitempty"`
	PostVerifySHA     string                      `json:"post_verify_sha,omitempty"`
	RecoveryRequired  bool                        `json:"recovery_required,omitempty"`
	SyncBase          string                      `json:"conflict_base,omitempty"`
	Attempts          int                         `json:"attempts"`
	Rotations         int                         `json:"checkpoint_rotations"`
	FixCycles         map[string]int              `json:"fix_cycles"`
	AdvisorUsed       bool                        `json:"advisor_used"`
	RunID             string                      `json:"run_id,omitempty"`
	Preflight         *Preflight                  `json:"preflight,omitempty"`
	Findings          []Finding                   `json:"findings,omitempty"`
	Summary           string                      `json:"implementation_summary,omitempty"`
	ReportedTests     []string                    `json:"reported_tests,omitempty"`
	Risks             []string                    `json:"remaining_risks,omitempty"`
	Decisions         []string                    `json:"decisions,omitempty"`
	Blocker           *Blocker                    `json:"blocker,omitempty"`
	Verification      *Verification               `json:"verification_retry_guard,omitempty"`
	Evidence          *Evidence                   `json:"evidence,omitempty"`
	ReviewProvenance  map[string]ReviewProvenance `json:"review_provenance,omitempty"`
	VisualRequired    *VisualRequirement          `json:"visual_required,omitempty"`
	Updated           time.Time                   `json:"updated"`
}

// VisualRequirement is a durable exact-head gate created when a preflight
// specialist could identify source repairs but could not inspect a rendered
// frame. It is not visual approval: final review must attach capture evidence
// and the named visual reviewer must complete before integration is allowed.
type VisualRequirement struct {
	Role   string `json:"role"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	Config string `json:"config"`
	Rules  string `json:"rules"`
	Reason string `json:"reason"`
}

// Preflight is portable so completed reader guidance survives a controller restart.
// Ready means the same source and policy may proceed to writer admission.
type Preflight struct {
	Phase       string           `json:"phase"`
	BaseSHA     string           `json:"base_sha"`
	HeadSHA     string           `json:"head_sha,omitempty"`
	Config      string           `json:"config"`
	Rules       string           `json:"rules"`
	Scope       string           `json:"scope_fingerprint,omitempty"`
	ReuseCount  int              `json:"reuse_count,omitempty"`
	ReuseReason string           `json:"reuse_reason,omitempty"`
	Completed   []string         `json:"completed,omitempty"`
	DirectFix   *DirectFixWaiver `json:"direct_fix_waiver,omitempty"`
}

// DirectFixWaiver is an exact-review, exact-head exception for one built-in
// pre-implementation role. It is separate from Completed: the role did not
// run for this head, and every other required role still must complete.
type DirectFixWaiver struct {
	Role        string `json:"role"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
	BaseSHA     string `json:"base_sha"`
	HeadSHA     string `json:"head_sha"`
	Config      string `json:"config"`
	Rules       string `json:"rules"`
	Scope       string `json:"scope_fingerprint"`
	Findings    string `json:"findings_fingerprint"`
}

// Guidance is encoded in the existing durable Decisions field so a correction
// survives attach without introducing a competing task-state schema while the
// capacity-state migration is in flight. The command ID makes delivery auditable.
type Guidance struct {
	CommandID string `json:"command_id"`
	SourceID  string `json:"source_task"`
	SourceSHA string `json:"source_head"`
	Operator  bool   `json:"operator,omitempty"`
	Head      string `json:"target_head,omitempty"`
	Base      string `json:"canonical_base,omitempty"`
	Config    string `json:"config,omitempty"`
	Rules     string `json:"rules,omitempty"`
	Pending   bool   `json:"pending_delivery,omitempty"`
	Delivered bool   `json:"delivered,omitempty"`
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

// EligibleGuidance keeps cross-task corrections compatible while making
// operator guidance one-shot exact-scope input. Before first checkout the
// durable base revision is the task's stable scope head.
func EligibleGuidance(t *Task, baseSHA, configHash, rules string) []Guidance {
	head := t.HeadSHA
	if head == "" {
		head = t.BaseSHA
	}
	var out []Guidance
	for _, item := range TaskGuidance(t) {
		if !item.Operator || (!item.Delivered && item.Base == baseSHA && item.Config == configHash && item.Rules == rules && (item.Head == head || item.Pending)) {
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

// QueueOperatorGuidance is deliberately scoped to the exact task checkpoint
// and active policy hashes. It shares the durable delivery/replay mechanism
// with cross-task guidance, but requires no synthetic source task.
func QueueOperatorGuidance(target *Task, commandID, head, baseSHA, configHash, rules, message string) error {
	if target == nil {
		return errors.New("operator guidance requires a target")
	}
	scopeHead := target.HeadSHA
	if scopeHead == "" {
		scopeHead = target.BaseSHA
	}
	if (target.State != Ready && target.State != Running && target.State != Fix && target.State != SyncRequired) || scopeHead != head || !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(head) || !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(baseSHA) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(configHash) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(rules) {
		return errors.New("operator guidance requires a READY, RUNNING, FIX, or SYNC_REQUIRED exact task head and policy scope")
	}
	message = strings.TrimSpace(message)
	if message == "" || len(message) > MaxGuidanceBytes || !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') || len(TaskGuidance(target)) >= MaxTaskGuidance {
		return errors.New("operator guidance must be bounded and unique")
	}
	for _, prior := range TaskGuidance(target) {
		if prior.Operator && prior.Head == head && prior.Base == baseSHA && prior.Config == configHash && prior.Rules == rules && prior.Text == message {
			return errors.New("duplicate operator guidance")
		}
	}
	encoded, err := json.Marshal(Guidance{CommandID: commandID, SourceID: "operator", SourceSHA: head, Operator: true, Head: head, Base: baseSHA, Config: configHash, Rules: rules, Text: message})
	if err != nil {
		return err
	}
	target.Decisions = append(target.Decisions, guidancePrefix+string(encoded))
	return nil
}

// PromptTask removes durable guidance records from the task projection embedded
// in a provider prompt. Eligible guidance is appended as a separate, scoped
// section by the role compiler; durable Decisions remain the audit history.
func PromptTask(t *Task) *Task {
	if t == nil {
		return nil
	}
	copy := *t
	copy.Decisions = make([]string, 0, len(t.Decisions))
	for _, decision := range t.Decisions {
		if !strings.HasPrefix(decision, guidancePrefix) {
			copy.Decisions = append(copy.Decisions, decision)
		}
	}
	return &copy
}

func rewriteGuidance(t *Task, change func(Guidance, int) Guidance) {
	seen := 0
	for i, decision := range t.Decisions {
		if !strings.HasPrefix(decision, guidancePrefix) {
			continue
		}
		var item Guidance
		if json.Unmarshal([]byte(strings.TrimPrefix(decision, guidancePrefix)), &item) != nil {
			continue
		}
		item = change(item, seen)
		seen++
		encoded, err := json.Marshal(item)
		if err == nil {
			t.Decisions[i] = guidancePrefix + string(encoded)
		}
	}
}

// CarryLateOperatorGuidance authorizes a one-time delivery only for guidance
// accepted during the implementer invocation that just checkpointed. It keeps
// the original head and policy provenance for audit while allowing that bounded
// supervisor-owned replay to cross the newly published checkpoint.
func CarryLateOperatorGuidance(t *Task, guidanceAtStart int) {
	rewriteGuidance(t, func(item Guidance, index int) Guidance {
		if index >= guidanceAtStart && item.Operator && !item.Delivered && item.Head != t.HeadSHA {
			item.Pending = true
		}
		return item
	})
}

// MarkOperatorGuidanceDelivered records the one-time provider delivery before
// starting that provider invocation, so restart cannot replay it a second time.
func MarkOperatorGuidanceDelivered(t *Task, guidance []Guidance) {
	ids := map[string]bool{}
	for _, item := range guidance {
		if item.Operator {
			ids[item.CommandID] = true
		}
	}
	rewriteGuidance(t, func(item Guidance, _ int) Guidance {
		if item.Operator && ids[item.CommandID] {
			item.Delivered = true
		}
		return item
	})
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
	Base               string                       `json:"base"`
	Head               string                       `json:"head"`
	Config             string                       `json:"config"`
	Rules              string                       `json:"rules"`
	Checks             []string                     `json:"checks"`
	Visual             *VisualEvidence              `json:"visual,omitempty"`
	Reviews            map[string]string            `json:"reviews"`
	ReviewRoster       []string                     `json:"review_roster,omitempty"`
	ReviewRosterReason string                       `json:"review_roster_reason,omitempty"`
	ReviewScope        string                       `json:"review_scope,omitempty"`
	ReviewDispositions map[string]ReviewDisposition `json:"review_dispositions,omitempty"`
	IntegrationSHA     string                       `json:"integration_sha,omitempty"`
	IntegrationOwner   string                       `json:"integration_owner,omitempty"`
	At                 time.Time                    `json:"at"`
}

// ReviewProvenance survives the normal evidence reset between bounded FIX
// cycles. It records a completed, zero-finding review without treating that
// older review as exact-head evidence for a later merge.
type ReviewProvenance struct {
	Role        string    `json:"role"`
	Base        string    `json:"base"`
	Head        string    `json:"head"`
	Config      string    `json:"config"`
	Rules       string    `json:"rules"`
	Roster      []string  `json:"roster"`
	Scope       string    `json:"scope"`
	Provider    string    `json:"provider"`
	Runtime     string    `json:"runtime"`
	Summary     string    `json:"summary"`
	CompletedAt time.Time `json:"completed_at"`
}

// ReviewDisposition proves how each required final review role was satisfied.
// completed is exact-head; reused is an explicitly narrow policy exception
// whose source remains visible to operators and final integration.
type ReviewDisposition struct {
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	SourceHead  string `json:"source_head"`
	Runtime     string `json:"runtime"`
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

const (
	AreaFile      = "file"
	AreaDirectory = "directory"
	AreaUnknown   = "unknown"
)

// ImmutableAreas returns a copy of the plan-time ownership boundary. Legacy
// state may be reconstructed only before a task has started, because older
// runtimes could append changed paths to Areas after a checkpoint.
func ImmutableAreas(t *Task) ([]string, bool) {
	if t != nil && len(t.AssignedAreas) != 0 {
		return append([]string(nil), t.AssignedAreas...), true
	}
	if t != nil && t.HeadSHA == "" && (t.State == Planned || t.State == Ready) && len(t.Areas) != 0 {
		return append([]string(nil), t.Areas...), true
	}
	return nil, false
}

// ImmutableAreaKinds returns a complete copy of the classifications for an
// immutable assignment. Missing or invalid values are represented as unknown
// so callers fail closed until the planner supplies base-tree evidence.
func ImmutableAreaKinds(t *Task) map[string]string {
	if t == nil || len(t.AssignedAreas) == 0 {
		return nil
	}
	out := make(map[string]string, len(t.AssignedAreas))
	for _, area := range t.AssignedAreas {
		kind := t.AssignedAreaKinds[area]
		if kind != AreaFile && kind != AreaDirectory && kind != AreaUnknown {
			kind = AreaUnknown
		}
		out[area] = kind
	}
	return out
}

func validateAssignedAreas(t *Task) error {
	if len(t.AssignedAreas) == 0 {
		if len(t.AssignedAreaKinds) != 0 {
			return errors.New("assigned area kinds without assigned areas")
		}
		return nil
	}
	seen := make(map[string]bool, len(t.AssignedAreas))
	for _, area := range t.AssignedAreas {
		if strings.TrimSpace(area) == "" || seen[area] {
			return errors.New("invalid assigned area")
		}
		seen[area] = true
		kind, ok := t.AssignedAreaKinds[area]
		if !ok || (kind != AreaFile && kind != AreaDirectory && kind != AreaUnknown) {
			return errors.New("invalid assigned area kind")
		}
	}
	for area := range t.AssignedAreaKinds {
		if !seen[area] {
			return errors.New("assigned area kind outside assigned areas")
		}
	}
	return nil
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
	if s.Schema <= 6 {
		// Version 7 makes the original task boundary durable. It can only be
		// reconstructed from Areas for work that has not started; after a
		// checkpoint Areas might contain legacy verification output. Historical
		// state has no portable base-tree metadata, so every recovered kind is
		// explicitly unknown rather than guessed from path spelling.
		for _, task := range s.Tasks {
			if task == nil {
				continue
			}
			if len(task.AssignedAreas) == 0 {
				areas, ok := ImmutableAreas(task)
				if !ok {
					continue
				}
				task.AssignedAreas = areas
			}
			if task.AssignedAreaKinds == nil {
				task.AssignedAreaKinds = make(map[string]string, len(task.AssignedAreas))
			}
			for _, area := range task.AssignedAreas {
				kind := task.AssignedAreaKinds[area]
				if kind != AreaFile && kind != AreaDirectory && kind != AreaUnknown {
					task.AssignedAreaKinds[area] = AreaUnknown
				}
			}
		}
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
		if err := validateAssignedAreas(t); err != nil {
			return nil, false, err
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
		if v := t.VisualRequired; v != nil {
			if v.Role == "" || !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(v.Role) ||
				!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(v.Base) ||
				!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(v.Head) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.Config) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.Rules) || strings.TrimSpace(v.Reason) == "" {
				return nil, false, errors.New("invalid visual requirement")
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
			if w := p.DirectFix; w != nil {
				if w.Role != "designer" || w.Disposition != "waived" || strings.TrimSpace(w.Reason) == "" ||
					!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(w.BaseSHA) ||
					!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(w.HeadSHA) ||
					!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(w.Config) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(w.Rules) ||
					!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(w.Scope) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(w.Findings) {
					return nil, false, errors.New("invalid direct FIX preflight waiver")
				}
			}
		}
		if t.Evidence != nil && t.Evidence.Visual != nil {
			v := t.Evidence.Visual
			if v.Head != t.Evidence.Head || v.Config != t.Evidence.Config ||
				!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(v.Head) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.Config) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.ManifestSHA256) ||
				len(v.Summary) > 1000 || len(v.Artifacts) < 1 || len(v.Artifacts) > MaxVisualEvidenceArtifacts ||
				v.Manifest != "visual-evidence/"+id+"/"+v.Head+"-"+v.Config[:16]+"/manifest.json" {
				return nil, false, errors.New("invalid visual evidence reference")
			}
			for _, artifact := range v.Artifacts {
				if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,119}\.(png|jpg|jpeg|txt|json)$`).MatchString(artifact.Path) || strings.Contains(artifact.Path, "..") || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(artifact.SHA256) {
					return nil, false, errors.New("invalid visual artifact reference")
				}
			}
		}
		for role, provenance := range t.ReviewProvenance {
			if err := validReviewProvenance(role, provenance); err != nil {
				return nil, false, err
			}
		}
		if t.Evidence != nil {
			if t.Evidence.ReviewScope != "" && !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(t.Evidence.ReviewScope) {
				return nil, false, errors.New("invalid review scope")
			}
			for role, disposition := range t.Evidence.ReviewDispositions {
				if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(role) ||
					(disposition.Disposition != "completed" && disposition.Disposition != "reused") ||
					!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(disposition.SourceHead) ||
					strings.TrimSpace(disposition.Runtime) == "" || len(disposition.Runtime) > 160 || len(disposition.Reason) > 500 {
					return nil, false, errors.New("invalid review disposition")
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

func validReviewProvenance(role string, provenance ReviewProvenance) error {
	if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(role) || provenance.Role != role ||
		!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(provenance.Base) ||
		!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(provenance.Head) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(provenance.Config) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(provenance.Rules) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(provenance.Scope) ||
		len(provenance.Roster) == 0 || strings.TrimSpace(provenance.Provider) == "" || len(provenance.Provider) > 80 ||
		strings.TrimSpace(provenance.Runtime) == "" || len(provenance.Runtime) > 160 || len(provenance.Summary) > 4000 || provenance.CompletedAt.IsZero() {
		return errors.New("invalid review provenance")
	}
	seen := map[string]bool{}
	for _, name := range provenance.Roster {
		if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(name) || seen[name] {
			return errors.New("invalid review provenance roster")
		}
		seen[name] = true
	}
	return nil
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
	baseline := runnableWhereOrdered(s, active, limit, eligible, Ordered(s))
	ordered := Ordered(s)
	// A task with a durable draft PR has already consumed a writer slice and may
	// be holding up dependent work. Finish that bounded FIX or continuation
	// before starting newly provisioned work when both are otherwise runnable.
	// Task state has no durable ready-since value, so this intentionally does not
	// add a starvation timer; IDs remain the deterministic tie-breaker.
	sort.SliceStable(ordered, func(i, j int) bool {
		return runnablePriority(s, ordered[i]) > runnablePriority(s, ordered[j])
	})
	prioritized := runnableWhereOrdered(s, active, limit, eligible, ordered)
	if len(prioritized) < len(baseline) {
		return baseline
	}
	return prioritized
}

func runnableWhereOrdered(s *Snapshot, active map[string]bool, limit int, eligible func(*Task) bool, ordered []*Task) []*Task {
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
	for _, t := range ordered {
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

func runnablePriority(s *Snapshot, task *Task) int {
	if task == nil || task.PR == 0 || (task.State != Ready && task.State != Fix) {
		return 0
	}
	for _, candidate := range s.Tasks {
		if !waitingOnDependency(candidate) {
			continue
		}
		for _, dependency := range candidate.Dependencies {
			if dependency == task.ID {
				return 2
			}
		}
	}
	return 1
}

func waitingOnDependency(task *Task) bool {
	return task != nil && (task.State == Planned || task.State == Ready || task.State == Fix)
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
