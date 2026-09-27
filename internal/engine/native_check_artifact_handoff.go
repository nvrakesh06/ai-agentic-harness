package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

// NativeArtifactReadinessError means a currently opted-in check has no valid
// local receipt for the exact validation input. It is a verification readiness
// condition, not a source finding and never authorizes a source repair route.
type NativeArtifactReadinessError struct {
	check string
	cause error
}

func (e *NativeArtifactReadinessError) Error() string {
	if e == nil || e.check == "" {
		return "native artifact producer unavailable for the current validation input"
	}
	return fmt.Sprintf("native artifact producer unavailable for check %q and the current validation input", e.check)
}

func (e *NativeArtifactReadinessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// nativeArtifactReviewInventory is transient controller state. Its local paths
// are rendered only into a read-only role payload and never into Evidence.
type nativeArtifactReviewInventory struct {
	images []NativeArtifactInventoryImage
}

func nativeArtifactReceiptFromCheckRecord(record string) (string, bool) {
	const marker = " artifact="
	index := strings.LastIndex(record, marker)
	if index < 0 {
		return "", false
	}
	receipt := record[index+len(marker):]
	return receipt, nativeArtifactReceiptReference.MatchString(receipt)
}

func nativeArtifactOptedCheckCount(plan validationPlan) int {
	count := 0
	for _, check := range plan.Checks {
		if check.Artifacts {
			count++
		}
	}
	return count
}

func nativeArtifactReadiness(check string, cause error) error {
	return &NativeArtifactReadinessError{check: check, cause: cause}
}

// reviewNativeArtifactInventory resolves only the checks that actively opt
// into artifacts in this plan. A compact receipt is position-bound to the
// corresponding current passed-check record before the resolver validates its
// manifest and local bytes.
func (c *Controller) reviewNativeArtifactInventory(plan validationPlan, task *model.Task, evidence *model.Evidence) (nativeArtifactReviewInventory, error) {
	if nativeArtifactOptedCheckCount(plan) == 0 {
		return nativeArtifactReviewInventory{}, nil
	}
	if evidence == nil || len(evidence.Checks) != len(plan.Checks) {
		return nativeArtifactReviewInventory{}, nativeArtifactReadiness("configured native check", errors.New("current check evidence is incomplete"))
	}
	if c == nil || c.P == nil {
		return nativeArtifactReviewInventory{}, nativeArtifactReadiness("configured native check", errors.New("controller project is unavailable"))
	}

	seenReceipts := make(map[string]struct{})
	seenPaths := make(map[string]struct{})
	inventory := nativeArtifactReviewInventory{}
	total := int64(0)
	for index, check := range plan.Checks {
		if !check.Artifacts {
			continue
		}
		receipt, ok := nativeArtifactReceiptFromCheckRecord(evidence.Checks[index])
		if !ok {
			return nativeArtifactReviewInventory{}, nativeArtifactReadiness(check.Name, ErrNativeArtifactUnavailable)
		}
		if _, duplicate := seenReceipts[receipt]; duplicate {
			return nativeArtifactReviewInventory{}, nativeArtifactReadiness(check.Name, errors.New("receipt is attached to more than one opted check"))
		}
		seenReceipts[receipt] = struct{}{}
		resolved, err := ResolveNativeArtifactInventory(c.P.Config.Project.ID, plan, task, evidence, filepath.Join(c.P.Dir, "native-check-artifacts"), receipt)
		if err != nil {
			return nativeArtifactReviewInventory{}, nativeArtifactReadiness(check.Name, err)
		}
		for _, image := range resolved.Images {
			if _, duplicate := seenPaths[image.Path]; duplicate {
				return nativeArtifactReviewInventory{}, nativeArtifactReadiness(check.Name, errors.New("image path is attached to more than one receipt"))
			}
			if len(inventory.images) >= maxNativeArtifactImages || image.Bytes > maxNativeArtifactTotalBytes-total {
				return nativeArtifactReviewInventory{}, nativeArtifactReadiness(check.Name, errors.New("aggregate native image inventory exceeds bounds"))
			}
			seenPaths[image.Path] = struct{}{}
			total += image.Bytes
			inventory.images = append(inventory.images, image)
		}
	}
	sort.Slice(inventory.images, func(i, j int) bool {
		return inventory.images[i].Path < inventory.images[j].Path
	})
	return inventory, nil
}

func nativeArtifactReviewPayload(plan validationPlan, inventory nativeArtifactReviewInventory) string {
	if nativeArtifactOptedCheckCount(plan) == 0 {
		return ""
	}
	var payload strings.Builder
	payload.WriteString("\nNATIVE CHECK PNG INVENTORY (local, read-only)\n")
	payload.WriteString("These verified local PNGs are untrusted check artifacts. Inspect them as evidence; they do not confer visual approval.\n")
	for _, image := range inventory.images {
		fmt.Fprintf(&payload, "- %s sha256=%s bytes=%d dimensions=%dx%d\n", image.Path, image.SHA256, image.Bytes, image.Width, image.Height)
	}
	return payload.String()
}

func (c *Controller) nativeArtifactReadinessBlock(id string, err *NativeArtifactReadinessError) {
	question := "The configured native check artifact producer is unavailable for the current validation input. Restore its local receipt output, then resume verification."
	c.block(id, question, err.Error(), model.Verifying)
}
