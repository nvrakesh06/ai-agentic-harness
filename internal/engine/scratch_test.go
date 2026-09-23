package engine_test

import (
	"context"
	"github.com/nvrakesh06/ai-agentic-harness/internal/demo"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerScratchToolingNeverBecomesCheckpointedSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	f.Provider.ScratchTooling = map[string]bool{"dashboard": true}
	seedReadyTask(t, ctx, f, "dashboard")
	task := runUntilTaskState(t, ctx, f, "dashboard", model.Done)
	if _, err = os.Stat(filepath.Join(f.P.TaskPath(task), ".tmp-npm")); !os.IsNotExist(err) {
		t.Fatalf("worker tooling appeared in source worktree: %v", err)
	}
	if _, err = os.Stat(f.P.TaskScratchPath(task)); !os.IsNotExist(err) {
		t.Fatalf("completed task scratch was not cleaned: %v", err)
	}
}

func TestAttachKeepsTaskScratchMappingOnTheSameMachine(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	seedReadyTask(t, ctx, f, "resume-scratch")
	task := &model.Task{ID: "resume-scratch"}
	scratch := f.P.TaskScratchPath(task)
	marker := filepath.Join(scratch, "npm-cache", "cache-marker")
	if err = os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(marker, []byte("local cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.P.DB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := f.Open(ctx, f.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.DB.Close()
	if err = reopened.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reopened.TaskScratchPath(task); got != scratch {
		t.Fatalf("scratch mapping changed after attach: got %q want %q", got, scratch)
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("attach lost task scratch cache: %v", err)
	}
}

func TestTaskScratchCleanupRejectsCorruptTaskPath(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	outside := filepath.Join(f.P.Dir, "outside")
	marker := filepath.Join(outside, "marker")
	if err = os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(marker, []byte("must remain"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := &model.Task{ID: ".." + string(filepath.Separator) + "outside"}
	if err = f.P.RemoveTaskScratch(corrupt); err == nil {
		t.Fatal("corrupt task path was accepted for scratch cleanup")
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("unsafe scratch cleanup removed outside content: %v", err)
	}
}

func TestTaskScratchPathRejectsCorruptTaskIdentifierBeforeCreation(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	corrupt := &model.Task{ID: ".." + string(filepath.Separator) + "outside"}
	if _, err = f.P.ValidTaskScratchPath(corrupt); err == nil {
		t.Fatal("corrupt task identifier was accepted for scratch creation")
	}
}

func TestTaskScratchCleanupRejectsSiblingJunction(t *testing.T) {
	ctx := context.Background()
	f, err := demo.New(ctx, t.TempDir(), []string{"git", "diff", "--exit-code"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.P.DB.Close()
	root := filepath.Join(f.P.Dir, "scratch")
	sibling := filepath.Join(root, "sibling")
	marker := filepath.Join(sibling, "marker")
	if err = os.MkdirAll(sibling, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(marker, []byte("must remain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(sibling, filepath.Join(root, "task")); err != nil {
		t.Skipf("symlink fixture unavailable on this machine: %v", err)
	}
	if err = f.P.RemoveTaskScratch(&model.Task{ID: "task"}); err == nil {
		t.Fatal("sibling scratch junction was accepted for cleanup")
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatalf("scratch cleanup removed sibling content: %v", err)
	}
}
