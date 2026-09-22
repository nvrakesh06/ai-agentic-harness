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
)

const Version = "1.0.0"
const StateSchema = 1
const RulesVersion = 1
const RoleSchema = 1

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
	Findings         []Finding      `json:"findings,omitempty"`
	Summary          string         `json:"implementation_summary,omitempty"`
	ReportedTests    []string       `json:"reported_tests,omitempty"`
	Risks            []string       `json:"remaining_risks,omitempty"`
	Decisions        []string       `json:"decisions,omitempty"`
	Blocker          *Blocker       `json:"blocker,omitempty"`
	Evidence         *Evidence      `json:"evidence,omitempty"`
	Updated          time.Time      `json:"updated"`
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
	Base             string            `json:"base"`
	Head             string            `json:"head"`
	Config           string            `json:"config"`
	Rules            string            `json:"rules"`
	Checks           []string          `json:"checks"`
	Reviews          map[string]string `json:"reviews"`
	IntegrationSHA   string            `json:"integration_sha,omitempty"`
	IntegrationOwner string            `json:"integration_owner,omitempty"`
	At               time.Time         `json:"at"`
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
	ID         string    `json:"id"`
	Task       string    `json:"task"`
	Role       string    `json:"role"`
	Provider   string    `json:"provider"`
	Capability string    `json:"capability"`
	Version    string    `json:"version"`
	RulesHash  string    `json:"rules_hash"`
	Started    time.Time `json:"started"`
	DurationMS int64     `json:"duration_ms"`
	Outcome    string    `json:"outcome"`
	Epoch      uint64    `json:"epoch"`
}
type Snapshot struct {
	Schema             int                   `json:"state_schema"`
	CreatedBy          string                `json:"created_by_version"`
	Project            string                `json:"project"`
	Revision           uint64                `json:"revision"`
	Controller         Lease                 `json:"controller"`
	Objectives         map[string]*Objective `json:"objectives"`
	Tasks              map[string]*Task      `json:"tasks"`
	Runs               []Run                 `json:"runs,omitempty"`
	Applied            map[string]bool       `json:"applied_commands"`
	Improvements       []string              `json:"improvement_candidates,omitempty"`
	IntegrationBlocked string                `json:"integration_blocked,omitempty"`
}

func NewSnapshot(project string) *Snapshot {
	return &Snapshot{Schema: StateSchema, CreatedBy: Version, Project: project,
		Objectives: map[string]*Objective{}, Tasks: map[string]*Task{}, Applied: map[string]bool{}}
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
	migrated := s.Schema == 0
	if migrated {
		s.Schema = 1
		if s.CreatedBy == "" {
			s.CreatedBy = Version
		}
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
		if active[t.ID] || (t.State != Ready && t.State != Fix) {
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
