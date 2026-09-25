package gitx

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// AreaKind is the immutable interpretation of one task area at its planning
// base. It deliberately describes intent, rather than the current filesystem:
// a tracked file remains exact even if a worker replaces it with a directory.
type AreaKind string

const (
	AreaTrackedFile  AreaKind = "tracked-file"
	AreaExplicitFile AreaKind = "explicit-file"
	AreaDirectory    AreaKind = "directory-intent"
)

// Area is a canonical task boundary. Pattern is always slash-separated and is
// suitable for comparing with paths reported by Git.
type Area struct {
	Pattern string
	Kind    AreaKind
}

// AreaError identifies an invalid or ambiguous task-area expression. Callers
// must not turn this into a broad match; rejecting it is intentional.
type AreaError struct {
	Area   string
	Reason string
}

func (e *AreaError) Error() string {
	if e.Area == "" {
		return "invalid task area: " + e.Reason
	}
	return fmt.Sprintf("invalid task area %q: %s", e.Area, e.Reason)
}

// ScopeError identifies changed paths that are outside every assigned area.
// Paths are normalized and sorted so that worker and supervisor diagnostics are
// stable across operating systems.
type ScopeError struct{ Paths []string }

func (e *ScopeError) Error() string {
	if len(e.Paths) == 0 {
		return "task scope has no assigned areas"
	}
	return "task changed paths outside its assigned areas: " + strings.Join(e.Paths, ", ")
}

// ClassifyAreasAtRef turns canonical task-area expressions into immutable
// boundary decisions at ref. Plain paths that name a blob are tracked files;
// absent plain paths are intentional exact-file assignments; paths naming a
// tree and explicit directory spellings (dir/ or dir/**) are directories.
//
// Only a terminal /** glob is accepted. Other glob syntax cannot be classified
// as a file or a directory without guessing, so it is rejected fail-closed.
func (g Git) ClassifyAreasAtRef(ctx context.Context, ref string, areas []string) ([]Area, error) {
	if len(areas) == 0 {
		return nil, &AreaError{Reason: "at least one area is required"}
	}
	base, err := g.SHA(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve task-area base %q: %w", ref, err)
	}
	classified := make([]Area, 0, len(areas))
	seen := make(map[string]struct{}, len(areas))
	for _, raw := range areas {
		pattern, explicitDirectory, err := canonicalArea(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[pattern]; ok {
			return nil, &AreaError{Area: raw, Reason: "duplicates another canonical area"}
		}
		seen[pattern] = struct{}{}

		kind := AreaExplicitFile
		objectType, exists, err := g.baseTreeObjectType(ctx, base, pattern)
		if err != nil {
			return nil, &AreaError{Area: raw, Reason: "read base tree: " + err.Error()}
		}
		if explicitDirectory {
			if exists && objectType == "blob" {
				return nil, &AreaError{Area: raw, Reason: "explicit directory conflicts with a tracked file"}
			}
			kind = AreaDirectory
		} else if exists {
			switch objectType {
			case "blob":
				kind = AreaTrackedFile
			case "tree":
				kind = AreaDirectory
			default:
				return nil, &AreaError{Area: raw, Reason: "base tree entry is neither a file nor a directory"}
			}
		}
		classified = append(classified, Area{Pattern: pattern, Kind: kind})
	}
	return classified, nil
}

// ValidateScopePaths verifies both tracked and newly-created paths against a
// prior classification. It makes no filesystem stat calls, which avoids a
// worker changing an on-disk path after it was classified.
func ValidateScopePaths(areas []Area, paths []string) error {
	if len(areas) == 0 {
		return &ScopeError{}
	}
	if err := validateAreas(areas); err != nil {
		return err
	}
	var rejected []string
	for _, raw := range paths {
		candidate, err := canonicalGitPath(raw)
		if err != nil {
			return err
		}
		allowed := false
		for _, area := range areas {
			if candidate == area.Pattern || (area.Kind == AreaDirectory && strings.HasPrefix(candidate, area.Pattern+"/")) {
				allowed = true
				break
			}
		}
		if !allowed {
			rejected = append(rejected, candidate)
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	sort.Strings(rejected)
	rejected = compactStrings(rejected)
	return &ScopeError{Paths: rejected}
}

// ValidateCheckpointScope checks every currently changed path before a caller
// stages it. It includes unstaged, staged, deleted, renamed (as both sides via
// --no-renames), and untracked files, so a new file cannot evade a directory
// boundary check.
func (g Git) ValidateCheckpointScope(ctx context.Context, worktree string, areas []Area) error {
	w := Git{Dir: worktree}
	paths, err := changedWorktreePaths(ctx, w)
	if err != nil {
		return err
	}
	return ValidateScopePaths(areas, paths)
}

// ValidateFullCheckpointScope is the checkpoint admission API. In addition to
// current dirty paths, it validates every committed change from immutableBase
// through the worktree's current HEAD. Call this immediately before Checkpoint:
// checking only the dirty worktree would miss an earlier out-of-area commit.
func (g Git) ValidateFullCheckpointScope(ctx context.Context, worktree, immutableBase string, areas []Area) error {
	w := Git{Dir: worktree}
	if err := w.ValidateCommitScope(ctx, immutableBase, "HEAD", areas); err != nil {
		return err
	}
	return g.ValidateCheckpointScope(ctx, worktree, areas)
}

// ValidateCommitScope verifies an imported checkpoint against the immutable
// base-tree classification. It is intended for an eventual integration gate.
func (g Git) ValidateCommitScope(ctx context.Context, base, head string, areas []Area) error {
	baseSHA, err := g.SHA(ctx, base)
	if err != nil {
		return fmt.Errorf("resolve scope base %q: %w", base, err)
	}
	headSHA, err := g.SHA(ctx, head)
	if err != nil {
		return fmt.Errorf("resolve scope head %q: %w", head, err)
	}
	if !g.Ancestor(ctx, baseSHA, headSHA) {
		return fmt.Errorf("scope head %s does not descend from immutable base %s", headSHA, baseSHA)
	}
	out, err := g.Run(ctx, "", "diff", "--no-renames", "--name-only", "-z", baseSHA+".."+headSHA)
	if err != nil {
		return err
	}
	return ValidateScopePaths(areas, splitGitPaths(out))
}

func (g Git) baseTreeObjectType(ctx context.Context, base, pattern string) (string, bool, error) {
	out, err := g.Run(ctx, "", "ls-tree", "-z", base, "--", pattern)
	if err != nil {
		return "", false, err
	}
	if out == "" {
		return "", false, nil
	}
	entry := strings.SplitN(out, "\x00", 2)[0]
	parts := strings.SplitN(entry, "\t", 2)
	if len(parts) != 2 || parts[1] != pattern {
		return "", false, errors.New("ambiguous base-tree entry")
	}
	fields := strings.Fields(parts[0])
	if len(fields) != 3 {
		return "", false, errors.New("malformed base-tree entry")
	}
	return fields[1], true, nil
}

func changedWorktreePaths(ctx context.Context, g Git) ([]string, error) {
	untracked, err := g.Run(ctx, "", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	unstaged, err := g.Run(ctx, "", "diff", "--no-renames", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, err
	}
	staged, err := g.Run(ctx, "", "diff", "--cached", "--no-renames", "--name-only", "-z")
	if err != nil {
		return nil, err
	}
	return splitGitPaths(untracked + "\x00" + unstaged + "\x00" + staged), nil
}

func canonicalArea(raw string) (string, bool, error) {
	if raw == "" {
		return "", false, &AreaError{Area: raw, Reason: "empty path"}
	}
	if raw != strings.TrimSpace(raw) {
		return "", false, &AreaError{Area: raw, Reason: "leading or trailing whitespace is ambiguous"}
	}
	unix := strings.ReplaceAll(raw, `\`, "/")
	explicitDirectory := strings.HasSuffix(unix, "/") || strings.HasSuffix(unix, "/**")
	if strings.ContainsAny(strings.TrimSuffix(unix, "/**"), "*?[") {
		return "", false, &AreaError{Area: raw, Reason: "only a terminal /** glob is supported"}
	}
	if strings.Contains(unix, "]") {
		return "", false, &AreaError{Area: raw, Reason: "only a terminal /** glob is supported"}
	}
	if strings.HasSuffix(unix, "/**") {
		unix = strings.TrimSuffix(unix, "/**")
	} else {
		unix = strings.TrimSuffix(unix, "/")
	}
	canonical, err := canonicalGitPath(unix)
	if err != nil {
		return "", false, &AreaError{Area: raw, Reason: err.Error()}
	}
	return canonical, explicitDirectory, nil
}

func canonicalGitPath(raw string) (string, error) {
	p := strings.ReplaceAll(raw, `\`, "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return "", errors.New("path must be repository-relative")
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes repository root")
	}
	return clean, nil
}

func splitGitPaths(raw string) []string {
	paths := make([]string, 0)
	for _, candidate := range strings.Split(raw, "\x00") {
		if candidate != "" {
			paths = append(paths, candidate)
		}
	}
	return paths
}

func validateAreas(areas []Area) error {
	seen := make(map[string]struct{}, len(areas))
	for _, area := range areas {
		if area.Kind != AreaTrackedFile && area.Kind != AreaExplicitFile && area.Kind != AreaDirectory {
			return &AreaError{Area: area.Pattern, Reason: "unknown immutable area kind"}
		}
		canonical, err := canonicalGitPath(area.Pattern)
		if err != nil || canonical != area.Pattern || strings.ContainsAny(area.Pattern, "*?[]") {
			return &AreaError{Area: area.Pattern, Reason: "non-canonical immutable area pattern"}
		}
		if _, exists := seen[area.Pattern]; exists {
			return &AreaError{Area: area.Pattern, Reason: "duplicates another canonical area"}
		}
		seen[area.Pattern] = struct{}{}
	}
	return nil
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	end := 1
	for _, value := range values[1:] {
		if value != values[end-1] {
			values[end] = value
			end++
		}
	}
	return values[:end]
}
