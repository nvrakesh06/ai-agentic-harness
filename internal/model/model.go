// Package model defines portable state. It must never contain machine paths,
// process identifiers, credentials, or provider conversation history.
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const Version = "1.0.0"
const StateSchema = 11
const RulesVersion = 1
const RoleSchema = 1
const CapacityTransitionLimit = 20
const MaxTaskGuidance = 8
const MaxGuidanceBytes = 1600
const MaxScopeRecoveryReasonBytes = 1600
const MaxScopeRecoveryRecords = 8
const maxScopeRecoveryRecordBytes = 16 * 1024

// MaxVisualEvidenceArtifacts includes up to eight screenshots and one shared diagnostic log.
const MaxVisualEvidenceArtifacts = 9
const guidancePrefix = "AIH_GUIDANCE_V1:"
const scopeRecoveryPrefix = "AIH_SCOPE_RECOVERY_V1:"

// ScopeRecovery records an explicit operator authorization for a legacy task
// whose original immutable assignment was never persisted. It deliberately
// records the new contract rather than claiming to reconstruct history from
// mutable plan fields or changed paths.
type ScopeRecovery struct {
	CommandID    string   `json:"command_id"`
	StateRef     string   `json:"state_ref"`
	PolicyHash   string   `json:"policy_hash"`
	BaseSHA      string   `json:"base_sha"`
	HeadSHA      string   `json:"head_sha"`
	ContractHash string   `json:"contract_hash"`
	ManifestHash string   `json:"manifest_hash"`
	Areas        []string `json:"areas"`
	Dependencies []string `json:"additional_dependencies,omitempty"`
	Reason       string   `json:"reason"`
}

// ScopeRecoveryRecord returns the typed durable authorization for a command.
func ScopeRecoveryRecord(t *Task, commandID string) (ScopeRecovery, bool) {
	if t == nil {
		return ScopeRecovery{}, false
	}
	for _, decision := range t.Decisions {
		if !strings.HasPrefix(decision, scopeRecoveryPrefix) {
			continue
		}
		var record ScopeRecovery
		if json.Unmarshal([]byte(strings.TrimPrefix(decision, scopeRecoveryPrefix)), &record) == nil && record.CommandID == commandID {
			return record, true
		}
	}
	return ScopeRecovery{}, false
}

// RecordScopeRecovery stores a compact typed decision in the existing durable
// Decisions field. The receipt namespace is bounded independently from legacy
// decision history, which recovery must preserve verbatim.
func RecordScopeRecovery(t *Task, record ScopeRecovery) error {
	if t == nil || record.CommandID == "" || len(record.Reason) == 0 || len(record.Reason) > MaxScopeRecoveryReasonBytes || !utf8.ValidString(record.Reason) || strings.ContainsRune(record.Reason, '\x00') {
		return errors.New("invalid scope recovery decision")
	}
	if prior, ok := ScopeRecoveryRecord(t, record.CommandID); ok {
		if prior.ManifestHash == record.ManifestHash {
			return nil
		}
		return errors.New("scope recovery command ID already records a different manifest")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(encoded) > maxScopeRecoveryRecordBytes {
		return errors.New("scope recovery decision exceeds size limit")
	}
	if scopeRecoveryRecordCount(t) >= MaxScopeRecoveryRecords {
		return errors.New("scope recovery record limit reached")
	}
	t.Decisions = append(t.Decisions, scopeRecoveryPrefix+string(encoded))
	return nil
}

func scopeRecoveryRecordCount(t *Task) int {
	count := 0
	for _, decision := range t.Decisions {
		if strings.HasPrefix(decision, scopeRecoveryPrefix) {
			count++
		}
	}
	return count
}

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
	// Superseded is a terminal record of an explicitly replaced task. It is not
	// a successful completion: dependencies follow SupersededBy and only the
	// replacement's DONE state can satisfy them.
	Superseded State = "SUPERSEDED"
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
	SupersededBy      string                      `json:"superseded_by,omitempty"`
	Replan            *ReplanProvenance           `json:"replan_provenance,omitempty"`
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
	ReadOnlyRetries   map[string]ReadOnlyRetry    `json:"read_only_retries,omitempty"`
	Evidence          *Evidence                   `json:"evidence,omitempty"`
	ReviewProvenance  map[string]ReviewProvenance `json:"review_provenance,omitempty"`
	VisualRequired    *VisualRequirement          `json:"visual_required,omitempty"`
	Updated           time.Time                   `json:"updated"`
	Timing            *TaskTiming                 `json:"timing,omitempty"`
}

// ReadOnlyRetry fences the one narrower retry available to a timed-out
// preflight or review role. It binds the consumed budget to the
// exact task and policy inputs so attach cannot restart a full reader budget.
type ReadOnlyRetry struct {
	Stage            string `json:"stage"`
	Role             string `json:"role"`
	BaseSHA          string `json:"base_sha"`
	HeadSHA          string `json:"head_sha,omitempty"`
	Config           string `json:"config"`
	Rules            string `json:"rules"`
	Attempts         int    `json:"attempts"`
	RemainingSeconds int    `json:"remaining_seconds"`
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

// QueueRoutedFinding is supervisor-created guidance for a concrete finding
// transferred between immutable task areas. Unlike an operator correction it
// may cross objective boundaries, but still requires a durable source head and
// is replayed when it arrives during an in-flight implementation.
func QueueRoutedFinding(target, source *Task, commandID, message string) error {
	if target == nil || source == nil || target.ID == source.ID {
		return errors.New("routed finding guidance requires distinct tasks")
	}
	if target.State != Ready && target.State != Running && target.State != Fix {
		return errors.New("routed finding target must be READY, RUNNING, or FIX")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(source.HeadSHA) {
		return errors.New("routed finding source needs a durable code checkpoint")
	}
	message = strings.TrimSpace(message)
	if message == "" || len(message) > MaxGuidanceBytes || !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') {
		return errors.New("routed finding guidance must be bounded UTF-8")
	}
	for _, prior := range TaskGuidance(target) {
		if prior.CommandID == commandID {
			return nil
		}
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
	CheckID           string `json:"check_id,omitempty"`
	Classification    string `json:"classification,omitempty"`
	Attempts          int    `json:"attempts"`
	NativeOnly        bool   `json:"native_only"`
}
type Blocker struct {
	Question       string `json:"question"`
	Reason         string `json:"reason"`
	Impact         string `json:"impact"`
	Recommendation string `json:"recommendation,omitempty"`
	Resume         State  `json:"resume_state"`
	Origin         string `json:"origin,omitempty"`
}

const (
	// BlockerOriginVerificationOnly marks a supervisor-owned verification
	// interruption that introduced no product or implementation decision.
	BlockerOriginVerificationOnly = "verification-only"
	// BlockerOriginImplementerDecision marks a worker request for a human
	// product or implementation decision.
	BlockerOriginImplementerDecision = "implementer-decision"
	// BlockerOriginProviderAuthentication marks a confirmed provider CLI
	// authentication error. It resumes the preserved task stage after the
	// operator restores provider access.
	BlockerOriginProviderAuthentication = "provider-authentication"
)

type Finding struct {
	Severity         string `json:"severity"`
	Category         string `json:"category"`
	Location         string `json:"location"`
	Reason           string `json:"reason"`
	Resolution       string `json:"suggested_resolution"`
	Role             string `json:"role,omitempty"`
	Relevance        string `json:"relevance,omitempty"`
	BaselineSHA      string `json:"baseline_sha,omitempty"`
	BaselineEvidence string `json:"baseline_evidence,omitempty"`
}

const (
	FindingChanged  = "changed"
	FindingCausal   = "causal"
	FindingBaseline = "baseline"
	FindingUnknown  = "unknown"
)

type Evidence struct {
	Base               string                       `json:"base"`
	Head               string                       `json:"head"`
	Config             string                       `json:"config"`
	Rules              string                       `json:"rules"`
	Checks             []string                     `json:"checks"`
	ValidationGate     string                       `json:"validation_gate,omitempty"`
	ValidationReason   string                       `json:"validation_reason,omitempty"`
	ValidationInput    string                       `json:"validation_input,omitempty"`
	Toolchain          string                       `json:"toolchain,omitempty"`
	TestInputs         string                       `json:"test_inputs,omitempty"`
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
	SourceHead     string           `json:"source_head,omitempty"`
	Config         string           `json:"config"`
	Closure        string           `json:"closure,omitempty"`
	Runtime        string           `json:"runtime,omitempty"`
	ReuseReason    string           `json:"reuse_reason,omitempty"`
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
	ID                string      `json:"id"`
	Task              string      `json:"task"`
	Role              string      `json:"role"`
	Provider          string      `json:"provider"`
	Capability        string      `json:"capability"`
	EffectiveModel    string      `json:"effective_model"`
	Version           string      `json:"version"`
	RulesHash         string      `json:"rules_hash"`
	Started           time.Time   `json:"started"`
	DurationMS        int64       `json:"duration_ms"`
	Outcome           string      `json:"outcome"`
	Epoch             uint64      `json:"epoch"`
	Context           *RunContext `json:"context,omitempty"`
	DurationRecorded  bool        `json:"duration_recorded,omitempty"`
	DurationEstimated bool        `json:"duration_estimated,omitempty"`
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
	Schema     int                   `json:"state_schema"`
	CreatedBy  string                `json:"created_by_version"`
	Project    string                `json:"project"`
	Revision   uint64                `json:"revision"`
	Controller Lease                 `json:"controller"`
	Objectives map[string]*Objective `json:"objectives"`
	Backlog    []string              `json:"authorized_objective_backlog"`
	Capacity   Capacity              `json:"capacity"`
	Tasks      map[string]*Task      `json:"tasks"`
	Runs       []Run                 `json:"runs,omitempty"`
	Applied    map[string]bool       `json:"applied_commands"`
	// Always encode this initialized receipt ledger. Clone uses the portable JSON
	// representation, so omitting an empty ledger would turn it into nil before
	// the first accepted replan and lose the write-ready provenance invariant.
	Replans            map[string]ReplanReceipt `json:"replan_receipts"`
	Improvements       []string                 `json:"improvement_candidates,omitempty"`
	IntegrationBlocked string                   `json:"integration_blocked,omitempty"`
	IntegrationBatch   *IntegrationBatch        `json:"integration_batch,omitempty"`
}

// IntegrationBatch is a portable reservation for the deliberately small first
// batch integration mode. It records admission inputs only; it neither changes
// task lifecycle state nor authorizes publishing. A later orchestrator must
// still construct, verify, and publish the integrated tree under the usual
// fence.
type IntegrationBatch struct {
	ID           string                 `json:"id"`
	BaseSHA      string                 `json:"base_sha"`
	Config       string                 `json:"config"`
	Rules        string                 `json:"rules"`
	ReviewRoster []string               `json:"review_roster"`
	Tasks        []IntegrationBatchTask `json:"tasks"`
}

// ReplanProvenance records the bounded operator authorization that created a
// successor. It belongs only to the new task; originals retain their prior
// issue, PR, evidence, and decision history unchanged.
type ReplanProvenance struct {
	CommandID     string             `json:"command_id"`
	Reason        string             `json:"reason"`
	CandidateHead string             `json:"candidate_head,omitempty"`
	Sources       []ReplanCheckpoint `json:"sources"`
}
type ReplanCheckpoint struct {
	TaskID  string `json:"task_id"`
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
	Order   int    `json:"order"`
}

// ReplanReceipt binds an idempotent operator command to its exact manifest.
// Applied alone cannot safely distinguish a retry from a different request
// that reused a command ID after canonical policy advanced.
type ReplanReceipt struct {
	Digest        string `json:"digest"`
	ReplacementID string `json:"replacement_id"`
}

// IntegrationBatchTask keeps every member's exact reviewed head and scope.
// Review scopes are intentionally per-task: disjoint changes cannot share one
// scope fingerprint, even when they share the selected review roster.
type IntegrationBatchTask struct {
	ID          string   `json:"id"`
	HeadSHA     string   `json:"head_sha"`
	ReviewScope string   `json:"review_scope"`
	Paths       []string `json:"paths"`
}

func NewSnapshot(project string) *Snapshot {
	return &Snapshot{Schema: StateSchema, CreatedBy: Version, Project: project,
		Objectives: map[string]*Objective{}, Backlog: []string{}, Tasks: map[string]*Task{}, Applied: map[string]bool{}, Replans: map[string]ReplanReceipt{}}
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
	if s.Schema <= 7 {
		// Schema 8 makes review relevance durable. Historical findings did not
		// contain a supervisor-verifiable classification, so preserve them as
		// explicit unknowns rather than treating their absence as baseline proof.
		// Blocker origin was also not recorded before schema 8, so leave it empty
		// and never grant the verification-only continuation exception to history.
		for _, task := range s.Tasks {
			if task == nil {
				continue
			}
			for i := range task.Findings {
				if task.Findings[i].Relevance == "" {
					task.Findings[i].Relevance = FindingUnknown
				}
			}
			if task.Blocker != nil {
				task.Blocker.Origin = ""
			}
		}
	}
	if s.Schema <= 8 {
		// Schema 9 adds an optional batch reservation. Historical snapshots
		// cannot safely infer one from independently reviewed tasks: selection
		// depends on current immutable scopes and exact changed paths. Leave it
		// empty so the next supervisor makes a fresh, deterministic decision.
		s.IntegrationBatch = nil
	}
	if s.Schema <= 9 {
		// Schema 10 adds explicit task replacement links. Old snapshots have no
		// authority to infer a replacement, so preserve their lifecycle exactly.
		for _, task := range s.Tasks {
			if task != nil {
				task.SupersededBy = ""
			}
		}
	}
	if migrated {
		if s.Schema <= 10 {
			// Earlier records have no transition clock or pinned run context.
			// Never reconstruct them from Updated or the current task revision.
			for _, task := range s.Tasks {
				if task != nil {
					task.Timing = nil
				}
			}
			for i := range s.Runs {
				s.Runs[i].Context = nil
			}
		}
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
	if s.Replans == nil {
		s.Replans = map[string]ReplanReceipt{}
	}
	for id, receipt := range s.Replans {
		if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(id) || !s.Applied[id] || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(receipt.Digest) || !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(receipt.ReplacementID) || s.Tasks[receipt.ReplacementID] == nil {
			return nil, false, errors.New("invalid replan receipt")
		}
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
		if err := validateTaskTiming(t); err != nil {
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
		if len(t.ReadOnlyRetries) > 8 {
			return nil, false, errors.New("too many read-only retry guards")
		}
		for key, retry := range t.ReadOnlyRetries {
			if !regexp.MustCompile(`^(pre-implementation|review)/[a-z][a-z0-9_-]{0,63}$`).MatchString(key) ||
				(retry.Stage != "pre-implementation" && retry.Stage != "review") ||
				!regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(retry.Role) ||
				!regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(retry.BaseSHA) ||
				(retry.HeadSHA != "" && !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(retry.HeadSHA)) ||
				!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(retry.Config) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(retry.Rules) ||
				retry.Attempts < 1 || retry.Attempts > 2 || retry.RemainingSeconds < 0 || retry.RemainingSeconds > 86400 || key != retry.Stage+"/"+retry.Role {
				return nil, false, errors.New("invalid read-only retry guard")
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
			if v.SourceHead != "" && !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(v.SourceHead) {
				return nil, false, errors.New("invalid visual evidence source revision")
			}
			if (v.Closure != "" || v.ReuseReason != "") && (!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(v.Closure) || strings.TrimSpace(v.Runtime) == "" || len(v.Runtime) > 160) {
				return nil, false, errors.New("invalid visual evidence closure")
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
		if _, ok := edges[t.State]; !ok && t.State != Done && t.State != Superseded {
			return nil, false, fmt.Errorf("unknown task state %q", t.State)
		}
		if t.State == Superseded {
			if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(t.SupersededBy) || s.Tasks[t.SupersededBy] == nil || t.SupersededBy == t.ID {
				return nil, false, errors.New("superseded task has no valid replacement")
			}
		} else if t.SupersededBy != "" {
			return nil, false, errors.New("non-superseded task has replacement link")
		}
		if t.Replan != nil {
			if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(t.Replan.CommandID) || strings.TrimSpace(t.Replan.Reason) == "" || len(t.Replan.Reason) > 1600 || len(t.Replan.Sources) == 0 || len(t.Replan.Sources) > 8 || (t.Replan.CandidateHead != "" && !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(t.Replan.CandidateHead)) {
				return nil, false, errors.New("invalid replan provenance")
			}
			prior := 0
			for _, source := range t.Replan.Sources {
				if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,120}$`).MatchString(source.TaskID) || !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(source.BaseSHA) || !regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(source.HeadSHA) || source.BaseSHA == source.HeadSHA || source.Order <= prior {
					return nil, false, errors.New("invalid replan source provenance")
				}
				prior = source.Order
			}
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
	if s.IntegrationBatch != nil {
		if err := ValidateIntegrationBatch(&s, s.IntegrationBatch); err != nil {
			return nil, false, err
		}
	}
	if err := validateRunMetrics(s.Runs); err != nil {
		return nil, false, err
	}
	return &s, migrated, nil
}

const (
	MinIntegrationBatchTasks = 2
	MaxIntegrationBatchTasks = 3
)

type integrationBatchCandidate struct {
	task  *Task
	paths []string
}

// SelectIntegrationBatch returns the first deterministic pair or triple that
// is safe to reserve. It does not wait for another task to become ready: a
// pair is sufficient, while a singleton deliberately falls back to serial
// integration. changedPaths must be the exact base..head path list for each
// candidate; missing or malformed paths make that candidate ineligible.
func SelectIntegrationBatch(s *Snapshot, changedPaths map[string][]string) *IntegrationBatch {
	if s == nil || s.IntegrationBatch != nil {
		return nil
	}
	var candidates []integrationBatchCandidate
	for _, task := range Ordered(s) {
		paths := canonicalBatchPaths(changedPaths[task.ID])
		if !batchTaskEligible(s, task, paths) {
			continue
		}
		candidates = append(candidates, integrationBatchCandidate{task: task, paths: paths})
	}
	// Prefer the lexically first valid triple. If none exists, return the
	// lexically first valid pair immediately; an incompatible early task cannot
	// anchor and suppress a later independent pair.
	for size := MaxIntegrationBatchTasks; size >= MinIntegrationBatchTasks; size-- {
		for i := 0; i < len(candidates); i++ {
			selected := findBatchSelection(candidates, i+1, size, []integrationBatchCandidate{candidates[i]})
			if selected != nil {
				return newIntegrationBatch(s, selected)
			}
		}
	}
	return nil
}

func findBatchSelection(candidates []integrationBatchCandidate, start, size int, selected []integrationBatchCandidate) []integrationBatchCandidate {
	if len(selected) == size {
		return selected
	}
	for i := start; i < len(candidates); i++ {
		if !batchCandidateCompatible(selected, candidates[i]) {
			continue
		}
		if match := findBatchSelection(candidates, i+1, size, append(selected, candidates[i])); match != nil {
			return match
		}
	}
	return nil
}

func batchCandidateCompatible(selected []integrationBatchCandidate, candidate integrationBatchCandidate) bool {
	first := selected[0].task
	if candidate.task.BaseSHA != first.BaseSHA || candidate.task.Evidence.Config != first.Evidence.Config || candidate.task.Evidence.Rules != first.Evidence.Rules || !sameStrings(candidate.task.Evidence.ReviewRoster, first.Evidence.ReviewRoster) {
		return false
	}
	for _, member := range selected {
		if batchScopesConflict(batchAreas(member.task), batchAreas(candidate.task)) || anyStringUsed(stringsToSet(member.task.Domains), candidate.task.Domains) || anyStringUsed(stringsToSet(member.paths), candidate.paths) {
			return false
		}
	}
	return true
}

func newIntegrationBatch(s *Snapshot, selected []integrationBatchCandidate) *IntegrationBatch {
	first := selected[0].task
	batch := &IntegrationBatch{BaseSHA: first.BaseSHA, Config: first.Evidence.Config, Rules: first.Evidence.Rules, ReviewRoster: append([]string(nil), first.Evidence.ReviewRoster...)}
	for _, candidate := range selected {
		task := candidate.task
		batch.Tasks = append(batch.Tasks, IntegrationBatchTask{ID: task.ID, HeadSHA: task.HeadSHA, ReviewScope: task.Evidence.ReviewScope, Paths: append([]string(nil), candidate.paths...)})
	}
	batch.ID = integrationBatchID(batch)
	if ValidateIntegrationBatch(s, batch) != nil {
		return nil
	}
	return batch
}

// ReserveIntegrationBatch validates a newly selected manifest before making it
// durable. A stale or concurrent reservation is rejected rather than replaced.
func ReserveIntegrationBatch(s *Snapshot, batch *IntegrationBatch) error {
	if s == nil || s.IntegrationBatch != nil {
		return errors.New("integration batch already reserved or snapshot unavailable")
	}
	if err := ValidateIntegrationBatch(s, batch); err != nil {
		return err
	}
	s.IntegrationBatch = cloneIntegrationBatch(batch)
	return nil
}

// ValidateIntegrationBatch checks portable reservation identity. It cannot
// recompute Git path overlap, so callers must use SelectIntegrationBatch (or
// an equivalently strict engine selector) before reserving it.
func ValidateIntegrationBatch(s *Snapshot, batch *IntegrationBatch) error {
	if s == nil || batch == nil || len(batch.Tasks) < MinIntegrationBatchTasks || len(batch.Tasks) > MaxIntegrationBatchTasks ||
		!validSHA(batch.BaseSHA) || !validHash(batch.Config) || !validHash(batch.Rules) || !validRoleRoster(batch.ReviewRoster) {
		return errors.New("invalid integration batch manifest")
	}
	if batch.ID != integrationBatchID(batch) {
		return errors.New("invalid integration batch identity")
	}
	previous := ""
	usedDomains := map[string]bool{}
	usedAreas := []immutableBatchArea{}
	usedPaths := map[string]bool{}
	for _, member := range batch.Tasks {
		paths := canonicalBatchPaths(member.Paths)
		if member.ID <= previous || !validSHA(member.HeadSHA) || !validHash(member.ReviewScope) || paths == nil {
			return errors.New("invalid integration batch member")
		}
		previous = member.ID
		task := s.Tasks[member.ID]
		if task == nil || !batchTaskEligible(s, task, paths) || task.BaseSHA != batch.BaseSHA || task.HeadSHA != member.HeadSHA ||
			task.Evidence.Config != batch.Config || task.Evidence.Rules != batch.Rules || !sameStrings(task.Evidence.ReviewRoster, batch.ReviewRoster) || task.Evidence.ReviewScope != member.ReviewScope {
			return errors.New("integration batch member is no longer eligible")
		}
		areas := batchAreas(task)
		if batchScopesConflict(usedAreas, areas) || anyStringUsed(usedDomains, task.Domains) || anyStringUsed(usedPaths, paths) {
			return errors.New("integration batch members overlap")
		}
		usedAreas = append(usedAreas, areas...)
		markStrings(usedDomains, task.Domains)
		markStrings(usedPaths, paths)
	}
	return nil
}

type immutableBatchArea struct{ path, kind string }

func batchTaskEligible(s *Snapshot, task *Task, paths []string) bool {
	if task == nil || task.State != MergeReady || task.Risk != "low" || task.Evidence == nil || task.BaseSHA == "" || task.HeadSHA == "" ||
		task.Evidence.Base != task.BaseSHA || task.Evidence.Head != task.HeadSHA || !validHash(task.Evidence.Config) || !validHash(task.Evidence.Rules) ||
		!validRoleRoster(task.Evidence.ReviewRoster) || !validHash(task.Evidence.ReviewScope) || !batchValidationAccepted(task.Evidence) || !completedDependencies(s, task) {
		return false
	}
	if len(task.Evidence.ReviewDispositions) != len(task.Evidence.ReviewRoster) {
		return false
	}
	for _, role := range task.Evidence.ReviewRoster {
		disposition, ok := task.Evidence.ReviewDispositions[role]
		if !ok || disposition.Disposition != "completed" || disposition.SourceHead != task.HeadSHA || strings.TrimSpace(disposition.Runtime) == "" {
			return false
		}
	}
	areas := batchAreas(task)
	if len(areas) == 0 || len(task.Domains) == 0 || hasDuplicateOrBlank(task.Domains) {
		return false
	}
	if len(paths) == 0 || !batchPathsWithinAreas(paths, areas) {
		return false
	}
	return true
}

func batchValidationAccepted(evidence *Evidence) bool {
	if evidence == nil || (evidence.ValidationGate != "focused" && evidence.ValidationGate != "full") || !validHash(evidence.ValidationInput) || strings.TrimSpace(evidence.Toolchain) == "" || !validSHA(evidence.TestInputs) || len(evidence.Checks) == 0 {
		return false
	}
	for _, check := range evidence.Checks {
		if !passedNativeCheckRecord.MatchString(check) {
			return false
		}
	}
	return true
}

// Passed native checks are recorded by engine.passedCheckEvidence. Batch
// admission parses that closed record shape rather than trusting arbitrary
// strings that merely claim an exit result.
var passedNativeCheckRecord = regexp.MustCompile(`^stage=native check="(?:[^"\\]|\\.)+" command="(?:[^"\\]|\\.)+" command_id=[a-f0-9]{12} exit=0 pass_counts="(?:[^"\\]|\\.)+" stdout=(captured|empty) stdout_bytes=(0|[1-9][0-9]*) stdout_lines=(0|[1-9][0-9]*)$`)

func completedDependencies(s *Snapshot, task *Task) bool {
	for _, id := range task.Dependencies {
		if !DependencyDone(s, id) {
			return false
		}
	}
	return true
}

// DependencyDone resolves explicit replacement links without ever treating a
// superseded original as success. A corrupt replacement loop is unsatisfied.
func DependencyDone(s *Snapshot, id string) bool {
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		seen[id] = true
		t := s.Tasks[id]
		if t == nil {
			return false
		}
		switch t.State {
		case Done:
			return true
		case Superseded:
			id = t.SupersededBy
		default:
			return false
		}
	}
	return false
}

// ObjectiveComplete applies the same successor resolution used for task
// dependencies, so replacing a task cannot make an objective look complete
// until the independently verified successor reaches DONE.
func ObjectiveComplete(s *Snapshot, objectiveID string) bool {
	for _, task := range s.Tasks {
		if task.ObjectiveID == objectiveID && !DependencyDone(s, task.ID) {
			return false
		}
	}
	return true
}

func batchAreas(task *Task) []immutableBatchArea {
	areas, ok := ImmutableAreas(task)
	if !ok || len(areas) == 0 {
		return nil
	}
	kinds := ImmutableAreaKinds(task)
	if len(kinds) != len(areas) {
		return nil
	}
	out := make([]immutableBatchArea, 0, len(areas))
	for _, path := range areas {
		kind := kinds[path]
		if kind != AreaFile && kind != AreaDirectory || strings.TrimSpace(path) == "" {
			return nil
		}
		out = append(out, immutableBatchArea{path, kind})
	}
	return out
}

func batchScopesConflict(a, b []immutableBatchArea) bool {
	for _, left := range a {
		for _, right := range b {
			if left.path == right.path || (left.kind == AreaDirectory && strings.HasPrefix(right.path, left.path+"/")) || (right.kind == AreaDirectory && strings.HasPrefix(left.path, right.path+"/")) {
				return true
			}
		}
	}
	return false
}

func canonicalBatchPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := append([]string(nil), paths...)
	sort.Strings(out)
	for i, path := range out {
		if path == "" || path != strings.TrimSpace(path) || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || pathpkg.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, "../") || (i > 0 && path == out[i-1]) {
			return nil
		}
	}
	return out
}

func batchPathsWithinAreas(paths []string, areas []immutableBatchArea) bool {
	for _, path := range paths {
		matched := false
		for _, area := range areas {
			matched = path == area.path || (area.kind == AreaDirectory && strings.HasPrefix(path, area.path+"/"))
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func anyStringUsed(used map[string]bool, values []string) bool {
	for _, value := range values {
		if used[value] || strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}
func markStrings(used map[string]bool, values []string) {
	for _, value := range values {
		used[value] = true
	}
}
func stringsToSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	markStrings(set, values)
	return set
}
func hasDuplicateOrBlank(values []string) bool {
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}
func sameStrings(a, b []string) bool {
	return len(a) == len(b) && strings.Join(a, "\x00") == strings.Join(b, "\x00")
}
func validSHA(value string) bool {
	return regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`).MatchString(value)
}
func validHash(value string) bool { return regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(value) }
func validRoleRoster(roster []string) bool {
	if len(roster) == 0 || hasDuplicateOrBlank(roster) {
		return false
	}
	for _, role := range roster {
		if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`).MatchString(role) {
			return false
		}
	}
	return true
}

func integrationBatchID(batch *IntegrationBatch) string {
	payload, _ := json.Marshal(struct {
		Base, Config, Rules string
		Roster              []string
		Tasks               []IntegrationBatchTask
	}{batch.BaseSHA, batch.Config, batch.Rules, batch.ReviewRoster, batch.Tasks})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func cloneIntegrationBatch(batch *IntegrationBatch) *IntegrationBatch {
	copy := *batch
	copy.ReviewRoster = append([]string(nil), batch.ReviewRoster...)
	copy.Tasks = append([]IntegrationBatchTask(nil), batch.Tasks...)
	for i := range copy.Tasks {
		copy.Tasks[i].Paths = append([]string(nil), batch.Tasks[i].Paths...)
	}
	return &copy
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
	BlockWithOrigin(t, question, reason, resume, "")
}

// BlockWithOrigin preserves why a human unblock was required so later
// continuation policy does not infer it from prose after the blocker is gone.
func BlockWithOrigin(t *Task, question, reason string, resume State, origin string) {
	t.Blocker = &Blocker{Question: question, Reason: reason, Impact: "Only this task and its dependants wait.", Resume: resume, Origin: origin}
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
	// A human answer explicitly authorizes a new read-only attempt window after
	// a persisted deadline blocker. The old guard remains authoritative until
	// that acknowledgement; ordinary automatic retries never clear it.
	t.ReadOnlyRetries = nil
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
			if !DependencyDone(s, dep) {
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
