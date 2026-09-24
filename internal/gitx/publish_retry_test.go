package gitx

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPublishRetriesTransientTransportWithSameLeases(t *testing.T) {
	updates := []Update{{Branch: "aih-state", Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)}}
	args := []string{"push", "--atomic", "--force-with-lease=refs/heads/aih-state:" + updates[0].Old, "origin", updates[0].New + ":refs/heads/aih-state"}
	pushes := 0
	waits := []time.Duration{}
	err := publishWithRetry(context.Background(), updates, args,
		func(_ context.Context, got []string) error {
			pushes++
			if !reflect.DeepEqual(got, args) {
				t.Fatalf("retry changed fenced push args: %v", got)
			}
			if pushes < 3 {
				return errors.New("fatal: Could not resolve host: github.com")
			}
			return nil
		},
		func(_ context.Context, branch string) (string, error) {
			if branch != "aih-state" {
				t.Fatalf("unexpected ref %q", branch)
			}
			return updates[0].Old, nil
		},
		func(_ context.Context, delay time.Duration) error { waits = append(waits, delay); return nil }, 4)
	if err != nil || pushes != 3 || !reflect.DeepEqual(waits, []time.Duration{2 * time.Second, 4 * time.Second}) {
		t.Fatalf("transient publication did not recover: pushes=%d waits=%v err=%v", pushes, waits, err)
	}
}

func TestPublishRetriesRemoteInternalServerErrorWithSameLease(t *testing.T) {
	update := Update{Branch: "aih-state", Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)}
	pushes := 0
	err := publishWithRetry(context.Background(), []Update{update}, []string{"fenced"},
		func(context.Context, []string) error {
			pushes++
			if pushes == 1 {
				return errors.New("! ref [remote rejected] (Internal Server Error)\nremote: Internal Server Error")
			}
			return nil
		},
		func(context.Context, string) (string, error) { return update.Old, nil },
		func(context.Context, time.Duration) error { return nil }, 3)
	if err != nil || pushes != 2 {
		t.Fatalf("transient GitHub 500 stopped fenced publication: pushes=%d err=%v", pushes, err)
	}
}

func TestPublishLostAcknowledgementRequiresAllAtomicRefs(t *testing.T) {
	updates := []Update{
		{Branch: "main", Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)},
		{Branch: "aih-state", Old: strings.Repeat("c", 40), New: strings.Repeat("d", 40)},
	}
	for _, tc := range []struct {
		name    string
		heads   map[string]string
		wantErr bool
	}{
		{"both updated", map[string]string{"main": updates[0].New, "aih-state": updates[1].New}, false},
		{"only state updated", map[string]string{"main": updates[0].Old, "aih-state": updates[1].New}, true},
		{"diverged", map[string]string{"main": strings.Repeat("e", 40), "aih-state": updates[1].Old}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushes := 0
			err := publishWithRetry(context.Background(), updates, []string{"fenced"},
				func(context.Context, []string) error {
					pushes++
					return errors.New("fatal: Could not resolve host: github.com")
				},
				func(_ context.Context, branch string) (string, error) { return tc.heads[branch], nil },
				func(context.Context, time.Duration) error { t.Fatal("unsafe retry"); return nil }, 3)
			if (err != nil) != tc.wantErr || pushes != 1 {
				t.Fatalf("incorrect atomic reconciliation: pushes=%d err=%v", pushes, err)
			}
		})
	}
}

func TestPublishDoesNotRetryLeaseOrAuthenticationRejection(t *testing.T) {
	update := Update{Branch: "aih-state", Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)}
	pushes := 0
	err := publishWithRetry(context.Background(), []Update{update}, []string{"fenced"},
		func(context.Context, []string) error { pushes++; return errors.New("remote rejected: stale lease") },
		func(context.Context, string) (string, error) { return update.Old, nil },
		func(context.Context, time.Duration) error { t.Fatal("rejected lease retried"); return nil }, 3)
	if err == nil || pushes != 1 || !strings.Contains(err.Error(), "reconcile before retry") {
		t.Fatalf("rejected publication must fail closed: pushes=%d err=%v", pushes, err)
	}
}

func TestPublishConfirmsLostAcknowledgementAfterTransportRecovers(t *testing.T) {
	update := Update{Branch: "aih-state", Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)}
	pushes, reads := 0, 0
	err := publishWithRetry(context.Background(), []Update{update}, []string{"fenced"},
		func(context.Context, []string) error {
			pushes++
			if pushes == 1 {
				return errors.New("Could not resolve host: github.com")
			}
			return errors.New("remote rejected stale lease after successful first push")
		},
		func(context.Context, string) (string, error) {
			reads++
			if reads == 1 {
				return "", errors.New("Could not resolve host: github.com")
			}
			return update.New, nil
		},
		func(context.Context, time.Duration) error { return nil }, 3)
	if err != nil || pushes != 2 || reads != 2 {
		t.Fatalf("lost acknowledgement was not confirmed: pushes=%d reads=%d err=%v", pushes, reads, err)
	}
}

func TestPublishReportsExhaustedTransportRetries(t *testing.T) {
	update := Update{Branch: "aih-state", Old: strings.Repeat("a", 40), New: strings.Repeat("b", 40)}
	pushes := 0
	err := publishWithRetry(context.Background(), []Update{update}, []string{"fenced"},
		func(context.Context, []string) error {
			pushes++
			return errors.New("Could not resolve host: github.com")
		},
		func(context.Context, string) (string, error) {
			return "", errors.New("Could not resolve host: github.com")
		},
		func(context.Context, time.Duration) error { return nil }, 3)
	if err == nil || pushes != 3 || !strings.Contains(err.Error(), "aih attach then aih resume") {
		t.Fatalf("exhausted retry needs actionable recovery: pushes=%d err=%v", pushes, err)
	}
}
