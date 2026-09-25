// Package gitx contains deterministic Git operations. All ref publications use
// explicit expected revisions; background fetches cannot weaken their leases.
package gitx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Git struct{ Dir string }

func (g Git) Run(ctx context.Context, input string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	argv := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "user.name=AIH", "-c", "user.email=aih@localhost", "-c", "commit.gpgsign=false"}
	argv = append(argv, args...)
	env := []string{}
	for _, v := range os.Environ() {
		k := strings.ToUpper(strings.SplitN(v, "=", 2)[0])
		if strings.HasPrefix(k, "GIT_") {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	out, e := platform.Run(ctx, g.Dir, env, input, "git", argv...)
	if e != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), e, safety.Redact(out))
	}
	return strings.TrimRight(out, "\r\n"), nil
}
func Discover(ctx context.Context, dir string) (string, string, error) {
	g := Git{dir}
	root, e := g.Run(ctx, "", "rev-parse", "--show-toplevel")
	if e != nil {
		return "", "", e
	}
	remote, e := g.Run(ctx, "", "remote", "get-url", "origin")
	return root, remote, e
}
func OpenControl(ctx context.Context, dir, remote string) (Git, error) {
	g := Git{dir}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return g, e
	}
	if _, e := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(e) {
		if _, e = g.Run(ctx, "", "init", "--bare"); e != nil {
			return g, e
		}
		if _, e = g.Run(ctx, "", "remote", "add", "origin", remote); e != nil {
			return g, e
		}
	}
	actual, e := g.Run(ctx, "", "remote", "get-url", "origin")
	if e != nil {
		return g, e
	}
	if actual != remote {
		return g, errors.New("control repository remote does not match project")
	}
	// Fetch supplies an explicit refspec. Leave no configured tracking map:
	// otherwise a concurrent push also writes origin/* and races the serialized
	// fetch. Load fetches objects only; remote CAS uses explicit remote revisions.
	if mapping, err := g.Run(ctx, "", "config", "--local", "--get-all", "remote.origin.fetch"); err == nil && mapping != "" {
		if _, e = g.Run(ctx, "", "config", "--local", "--unset-all", "remote.origin.fetch"); e != nil {
			return g, e
		}
	}
	return g, nil
}
func (g Git) Fetch(ctx context.Context) error {
	return g.fetch(ctx, "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*")
}

func (g Git) fetch(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	// CLI observation/submission can fetch while the supervisor is running.
	// Protect both tracking refs and FETCH_HEAD across those local processes.
	lock, err := platform.AcquireContext(ctx, filepath.Join(g.Dir, "aih-fetch.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	_, err = g.Run(ctx, "", append([]string{"fetch"}, args...)...)
	return err
}
func (g Git) SHA(ctx context.Context, ref string) (string, error) {
	return g.Run(ctx, "", "rev-parse", "--verify", ref+"^{commit}")
}
func (g Git) Show(ctx context.Context, ref, name string) (string, error) {
	return g.Run(ctx, "", "show", ref+":"+name)
}
func (g Git) Files(ctx context.Context, ref, prefix string) ([]string, error) {
	args := []string{"ls-tree", "-r", "--name-only", ref}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, e := g.Run(ctx, "", args...)
	if out == "" {
		return nil, e
	}
	return strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), e
}
func (g Git) RemoteHead(ctx context.Context, branch string) (string, error) {
	out, e := g.Run(ctx, "", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if e != nil {
		return "", e
	}
	if out == "" {
		return "", nil
	}
	return strings.Fields(out)[0], nil
}
func (g Git) Load(ctx context.Context) (*model.Snapshot, string, error) {
	h, e := g.RemoteHead(ctx, "aih-state")
	if e != nil {
		return nil, "", e
	}
	if h == "" {
		return nil, "", os.ErrNotExist
	}
	if e = g.fetch(ctx, "origin", "refs/heads/aih-state"); e != nil {
		return nil, "", e
	}
	b, e := g.Show(ctx, h, "snapshot.json")
	if e != nil {
		return nil, "", e
	}
	s, _, e := model.Decode([]byte(b))
	return s, h, e
}
func (g Git) StateCommit(ctx context.Context, parent string, s *model.Snapshot) (string, error) {
	return g.snapshotCommit(ctx, parent, s, fmt.Sprintf("AIH state revision %d\n", s.Revision))
}

// LeaseCommit advances controller liveness without incrementing the
// user-significant state revision. It still writes the schema-1 snapshot so
// older compatible runtimes observe the renewed fence.
func (g Git) LeaseCommit(ctx context.Context, parent string, s *model.Snapshot) (string, error) {
	lease := s.Controller
	if parent == "" || lease.Owner == "" || lease.Machine == "" || lease.Epoch == 0 || !lease.Expires.After(lease.Heartbeat) {
		return "", errors.New("invalid durable lease renewal")
	}
	return g.snapshotCommit(ctx, parent, s, fmt.Sprintf("AIH lease renewal epoch %d\n", lease.Epoch))
}

func (g Git) snapshotCommit(ctx context.Context, parent string, s *model.Snapshot, message string) (string, error) {
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return "", e
	}
	if e = safety.Check(string(b)); e != nil {
		return "", e
	}
	if _, _, e = model.Decode(b); e != nil {
		return "", e
	}
	blob, e := g.Run(ctx, string(b), "hash-object", "-w", "--stdin")
	if e != nil {
		return "", e
	}
	tree, e := g.Run(ctx, "100644 blob "+blob+"\tsnapshot.json\n", "mktree")
	if e != nil {
		return "", e
	}
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return g.Run(ctx, message, args...)
}

type Update struct{ Branch, Old, New string }

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (g Git) Publish(ctx context.Context, updates []Update) error {
	if len(updates) == 0 {
		return errors.New("empty ref transaction")
	}
	args := []string{"push", "--atomic", "--porcelain"}
	refs := []string{}
	seen := map[string]bool{}
	for _, u := range updates {
		if u.Branch != "main" && u.Branch != "aih-state" && !strings.HasPrefix(u.Branch, "aih/") {
			return errors.New("publication outside AIH-owned refs")
		}
		if seen[u.Branch] || !shaPattern.MatchString(u.New) || (u.Old != "" && !shaPattern.MatchString(u.Old)) {
			return errors.New("invalid atomic ref transaction")
		}
		seen[u.Branch] = true
		if (u.Branch == "main" || u.Branch == "aih-state") && u.Old != "" && !g.Ancestor(ctx, u.Old, u.New) {
			return fmt.Errorf("ref %s must only advance", u.Branch)
		}
		if _, e := g.Run(ctx, "", "check-ref-format", "refs/heads/"+u.Branch); e != nil {
			return e
		}
		if u.Old == u.New {
			return fmt.Errorf("ref %s must change: Git drops no-op updates and would not check its lease", u.Branch)
		}
		args = append(args, "--force-with-lease=refs/heads/"+u.Branch+":"+u.Old)
		refs = append(refs, u.New+":refs/heads/"+u.Branch)
	}
	args = append(args, "origin")
	args = append(args, refs...)
	deadline, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return publishWithRetry(deadline, updates, args,
		func(ctx context.Context, args []string) error { _, err := g.Run(ctx, "", args...); return err },
		g.RemoteHead, waitForPublicationRetry, 20)
}

var transientGitTransportErrors = []string{
	"could not resolve host", "could not resolve hostname", "temporary failure in name resolution",
	"couldn't resolve host", "failed to connect", "could not connect to server",
	"connection timed out", "connection reset by peer", "network is unreachable",
	"internal server error", "the requested url returned error: 500",
	"the requested url returned error: 502", "the requested url returned error: 503",
	"the requested url returned error: 504", "bad gateway", "service unavailable", "gateway timeout",
}

func transientGitTransport(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range transientGitTransportErrors {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func waitForPublicationRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// publishWithRetry retains the exact push arguments, including every old-ref
// lease. A transient transport failure may have committed remotely, so all
// remote refs must be reconciled before a retry or a success acknowledgement.
func publishWithRetry(ctx context.Context, updates []Update, args []string,
	push func(context.Context, []string) error,
	remoteHead func(context.Context, string) (string, error),
	wait func(context.Context, time.Duration) error, attempts int) error {
	if attempts < 1 {
		return errors.New("atomic publication requires at least one attempt")
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = push(ctx, args)
		if last == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		allNew, sawNew := true, false
		for _, update := range updates {
			head, err := remoteHead(ctx, update.Branch)
			if err != nil {
				allNew = false
				if !transientGitTransport(err) {
					return fmt.Errorf("atomic publication could not reconcile %s: %w", update.Branch, err)
				}
				continue
			}
			if head != update.New {
				allNew = false
			}
			if head == update.New {
				sawNew = true
			}
			if head != update.Old && head != update.New {
				return fmt.Errorf("atomic publication ref %s diverged; reconcile before retry: %w", update.Branch, last)
			}
		}
		if allNew {
			return nil
		}
		if sawNew || !transientGitTransport(last) {
			return fmt.Errorf("atomic publication rejected or unconfirmed; reconcile before retry: %w", last)
		}
		if attempt == attempts-1 {
			break
		}
		delay := time.Duration(2<<min(attempt, 4)) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		log.Printf("AIH fenced publication transport retry %d/%d in %s: %s", attempt+2, attempts, delay, safety.Redact(last.Error()))
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
	return fmt.Errorf("atomic publication transport retry exhausted after %d attempts; run aih attach then aih resume after connectivity recovers: %w", attempts, last)
}
func (g Git) Worktree(ctx context.Context, path, branch, from string) error {
	if _, e := os.Stat(filepath.Join(path, ".git")); e == nil {
		current, e := (Git{path}).Run(ctx, "", "symbolic-ref", "--short", "HEAD")
		if e != nil {
			return e
		}
		if current != branch {
			return errors.New("worktree is on an unexpected branch")
		}
		return nil
	}
	if _, e := g.Run(ctx, "", "worktree", "prune"); e != nil {
		return e
	}
	if _, e := g.SHA(ctx, "refs/heads/"+branch); e == nil {
		_, e = g.Run(ctx, "", "worktree", "add", path, branch)
		return e
	}
	_, e := g.Run(ctx, "", "worktree", "add", "-b", branch, path, from)
	return e
}
func (g Git) Detached(ctx context.Context, path, ref string) error {
	_, e := g.Run(ctx, "", "worktree", "add", "--detach", path, ref)
	return e
}
func (g Git) RemoveWorktree(ctx context.Context, path string) error {
	_, e := g.Run(ctx, "", "worktree", "remove", path)
	return e
}

func (g Git) Checkpoint(ctx context.Context, path, task string) (string, error) {
	w := Git{path}
	files, e := w.Run(ctx, "", "ls-files", "--others", "--exclude-standard", "-z")
	if e != nil {
		return "", e
	}
	changed, e := w.Run(ctx, "", "diff", "--name-only", "-z", "HEAD")
	if e != nil {
		return "", e
	}
	for _, p := range strings.Split(files+"\x00"+changed, "\x00") {
		if p != "" {
			if e = safety.Path(p); e != nil {
				return "", fmt.Errorf("checkpoint candidate path %q rejected: %w", p, e)
			}
			content, re := os.ReadFile(filepath.Join(path, p))
			if re == nil {
				if e = safety.Check(string(content)); e != nil {
					return "", fmt.Errorf("checkpoint candidate %q contains prohibited secret-like content: %w", p, e)
				}
			}
		}
	}
	if _, e = w.Run(ctx, "", "add", "--all"); e != nil {
		return "", e
	}
	diff, e := w.Run(ctx, "", "diff", "--cached", "--binary")
	if e != nil {
		return "", e
	}
	if e = safety.Check(diff); e != nil {
		return "", e
	}
	if _, e = w.Run(ctx, "", "diff", "--cached", "--check"); e != nil {
		return "", fmt.Errorf("checkpoint contains whitespace errors or unresolved conflict markers: %w", e)
	}
	_, mergeErr := w.SHA(ctx, "MERGE_HEAD")
	if diff != "" || mergeErr == nil {
		if _, e = w.Run(ctx, "", "commit", "-m", "AIH checkpoint "+task); e != nil {
			return "", e
		}
	}
	return w.SHA(ctx, "HEAD")
}

// After an aborted rebase, prepare a merge with main for the disposable writer
// to resolve. SyncBase in durable state recreates this on a replacement machine.
func (g Git) PrepareMerge(ctx context.Context, dir, base string) error {
	w := Git{dir}
	if pending, e := w.SHA(ctx, "MERGE_HEAD"); e == nil {
		if pending != base {
			return errors.New("unexpected merge in task worktree")
		}
		return nil
	}
	if g.Ancestor(ctx, base, mustHead(ctx, w)) {
		return nil
	}
	_, e := w.Run(ctx, "", "merge", "--no-ff", "--no-commit", base)
	if e != nil {
		unmerged, ue := w.Run(ctx, "", "diff", "--name-only", "--diff-filter=U")
		if ue == nil && unmerged != "" {
			return nil
		}
	}
	return e
}
func mustHead(ctx context.Context, g Git) string { h, _ := g.SHA(ctx, "HEAD"); return h }
func (g Git) Rebase(ctx context.Context, path, base string) error {
	w := Git{path}
	status, e := w.Run(ctx, "", "status", "--porcelain")
	if e != nil {
		return e
	}
	if status != "" {
		return errors.New("cannot rebase a dirty task worktree")
	}
	// A task-owned merge may already contain the current base. Plain rebase
	// drops that merge and replays its earlier checkpoints, which can recreate
	// a conflict that the task owner has already resolved.
	head, e := w.SHA(ctx, "HEAD")
	if e != nil {
		return e
	}
	if w.Ancestor(ctx, base, head) {
		return nil
	}
	_, e = w.Run(ctx, "", "rebase", base)
	if e != nil {
		_, abortErr := w.Run(context.Background(), "", "rebase", "--abort")
		if abortErr != nil {
			return errors.Join(e, abortErr)
		}
	}
	return e
}
func (g Git) Diff(ctx context.Context, base, head string) (string, []string, error) {
	d, e := g.Run(ctx, "", "diff", "--no-ext-diff", base+"..."+head)
	if e != nil {
		return "", nil, e
	}
	// A rename must remain two changed paths here. The caller uses this list for
	// scope and validation planning, where seeing only the destination could
	// incorrectly classify a cross-package move as a focused local change.
	paths, e := g.Run(ctx, "", "diff", "--no-renames", "--name-only", base+"..."+head)
	if paths == "" {
		return d, nil, e
	}
	return d, strings.Split(strings.ReplaceAll(paths, "\r\n", "\n"), "\n"), e
}
func (g Git) Ancestor(ctx context.Context, base, head string) bool {
	_, e := g.Run(ctx, "", "merge-base", "--is-ancestor", base, head)
	return e == nil
}
func (g Git) MergeCommit(ctx context.Context, base, head, message string) (string, error) {
	if !g.Ancestor(ctx, base, head) {
		return "", errors.New("task is not synchronized to verified base")
	}
	tree, e := g.Run(ctx, "", "rev-parse", head+"^{tree}")
	if e != nil {
		return "", e
	}
	return g.Run(ctx, message+"\n", "commit-tree", tree, "-p", base, "-p", head)
}

// MergeHeads builds a caller-ordered chain of two or three merge commits without
// advancing a ref. Every head must independently descend from, and share, base.
// Git computes each merged tree and rejects a content conflict instead of choosing
// one head's version. The returned commit has every supplied head as an ancestor.
func (g Git) MergeHeads(ctx context.Context, base string, heads []string, message string) (string, error) {
	return g.mergeHeads(ctx, base, heads, message, g.mergeTreeWriteTreeSupported(ctx))
}

func (g Git) mergeHeads(ctx context.Context, base string, heads []string, message string, writeTree bool) (string, error) {
	if len(heads) < 2 || len(heads) > 3 {
		return "", errors.New("batch merge requires two or three task heads")
	}
	baseSHA, e := g.SHA(ctx, base)
	if e != nil {
		return "", e
	}
	resolved := make([]string, len(heads))
	seen := make(map[string]bool, len(heads))
	for i, head := range heads {
		resolved[i], e = g.SHA(ctx, head)
		if e != nil {
			return "", fmt.Errorf("batch merge head %d: %w", i+1, e)
		}
		if resolved[i] == baseSHA || seen[resolved[i]] {
			return "", errors.New("batch merge heads must be distinct descendants of the verified base")
		}
		seen[resolved[i]] = true
		mergeBase, err := g.Run(ctx, "", "merge-base", baseSHA, resolved[i])
		if err != nil || mergeBase != baseSHA {
			return "", fmt.Errorf("batch merge head %d is not synchronized to the verified base", i+1)
		}
	}
	for i := 0; i < len(resolved); i++ {
		for j := i + 1; j < len(resolved); j++ {
			mergeBase, err := g.Run(ctx, "", "merge-base", resolved[i], resolved[j])
			if err != nil || mergeBase != baseSHA {
				return "", errors.New("batch merge heads do not share the verified base")
			}
		}
	}

	current := baseSHA
	for i, head := range resolved {
		tree, err := g.mergedTree(ctx, current, head, writeTree)
		if err != nil {
			return "", fmt.Errorf("batch merge head %d conflicts with the preceding integration tree: %w", i+1, err)
		}
		if !shaPattern.MatchString(tree) {
			return "", fmt.Errorf("batch merge head %d produced an invalid tree", i+1)
		}
		current, err = g.Run(ctx, message+"\n", "commit-tree", tree, "-p", current, "-p", head)
		if err != nil {
			return "", err
		}
	}
	return current, nil
}

func (g Git) mergeTreeWriteTreeSupported(ctx context.Context) bool {
	version, err := g.Run(ctx, "", "version")
	if err != nil {
		return false
	}
	parts := strings.Fields(version)
	if len(parts) < 3 {
		return false
	}
	var major, minor int
	if _, err = fmt.Sscanf(parts[2], "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major > 2 || major == 2 && minor >= 38
}

func (g Git) mergedTree(ctx context.Context, current, head string, writeTree bool) (string, error) {
	if writeTree {
		return g.Run(ctx, "", "merge-tree", "--write-tree", current, head)
	}
	root, err := os.MkdirTemp("", "aih-merge-worktree-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "checkout")
	if _, err = g.Run(ctx, "", "worktree", "add", "--detach", path, current); err != nil {
		if cleanupErr := g.cleanupTemporaryWorktree(path, root); cleanupErr != nil {
			return "", errors.Join(err, cleanupErr)
		}
		return "", err
	}
	worktree := Git{Dir: path}
	cleanup := func() error {
		_, _ = worktree.Run(context.Background(), "", "merge", "--abort")
		return g.cleanupTemporaryWorktree(path, root)
	}
	if _, err = worktree.Run(ctx, "", "merge", "--no-commit", "--no-ff", head); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", errors.Join(err, cleanupErr)
		}
		return "", err
	}
	tree, err := worktree.Run(ctx, "", "write-tree")
	if err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return "", errors.Join(err, cleanupErr)
		}
		return "", err
	}
	if err = cleanup(); err != nil {
		return "", err
	}
	return tree, nil
}

// cleanupTemporaryWorktree removes a disposable merge worktree even if an
// interrupted add registered its metadata before returning an error. If Git
// cannot remove that registration itself, remove only our generated temporary
// root before pruning its now-stale administrative data.
func (g Git) cleanupTemporaryWorktree(path, root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, removeErr := g.Run(ctx, "", "worktree", "remove", "--force", path)
	if removeErr != nil {
		rootErr := os.RemoveAll(root)
		_, pruneErr := g.Run(ctx, "", "worktree", "prune")
		if rootErr != nil || pruneErr != nil {
			return errors.Join(removeErr, rootErr, pruneErr)
		}
		return nil
	}
	_, pruneErr := g.Run(ctx, "", "worktree", "prune")
	return pruneErr
}
