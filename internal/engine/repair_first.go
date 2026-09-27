package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/roles"
)

const maxReviewFindingReceipts = 64

// reviewFindingFingerprint identifies the normalized provider finding after the
// controller has assigned its role and relevance. It is stored only in
// supervisor-owned task state; providers never see or submit this receipt.
func reviewFindingFingerprint(finding model.Finding) string {
	payload, _ := json.Marshal(struct {
		Severity, Category, Location, Reason, Resolution, Role, Relevance, BaselineSHA, BaselineEvidence string
	}{finding.Severity, finding.Category, finding.Location, finding.Reason, finding.Resolution, finding.Role, finding.Relevance, finding.BaselineSHA, finding.BaselineEvidence})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func reviewFindingReceipts(source *model.Task, evidence *model.Evidence, findings []model.Finding, controllerSummary bool) []model.ReviewFindingProvenance {
	if source == nil || evidence == nil || evidence.Base == "" || evidence.Head == "" || evidence.Config == "" || evidence.Rules == "" {
		return nil
	}
	receipts := make([]model.ReviewFindingProvenance, 0, len(findings))
	for _, finding := range findings {
		if finding.Role == "" {
			continue
		}
		receipts = append(receipts, model.ReviewFindingProvenance{
			Finding: reviewFindingFingerprint(finding), SourceTask: source.ID,
			Base: evidence.Base, Head: evidence.Head, Config: evidence.Config, Rules: evidence.Rules,
			Role: finding.Role, ControllerSummary: controllerSummary,
		})
	}
	return receipts
}

func appendReviewFindingReceipts(task *model.Task, receipts []model.ReviewFindingProvenance) {
	if task == nil {
		return
	}
	for _, receipt := range receipts {
		if len(task.ReviewFindingProvenance) >= maxReviewFindingReceipts {
			// Receipt overflow must never drop a provider finding or make review
			// persistence fail. The missing receipt simply declines repair-first.
			task.ReviewFindingReceiptOverflow = true
			return
		}
		if !slices.ContainsFunc(task.ReviewFindingProvenance, func(current model.ReviewFindingProvenance) bool {
			return current == receipt
		}) {
			task.ReviewFindingProvenance = append(task.ReviewFindingProvenance, receipt)
		}
	}
}

// hasAttributedConcreteReviewFinding accepts only a retained, source-located
// high/critical defect from the same exact review input. A low/medium receipt
// must never authorize the controller's synthetic retry summary.
func hasAttributedConcreteReviewFinding(task *model.Task, evidence *model.Evidence, role string, effective config.Effective) bool {
	if task == nil || evidence == nil {
		return false
	}
	for _, finding := range task.Findings {
		if finding.Role != role || (finding.Severity != "critical" && finding.Severity != "high") ||
			(finding.Relevance != model.FindingChanged && finding.Relevance != model.FindingCausal) || !findingInTaskScope(task, finding) {
			continue
		}
		receipt, ok := receiptForFinding(task, finding, effective)
		if ok && !receipt.ControllerSummary && receipt.Base == evidence.Base && receipt.Head == evidence.Head {
			return true
		}
	}
	return false
}

func receiptForFinding(task *model.Task, finding model.Finding, effective config.Effective) (model.ReviewFindingProvenance, bool) {
	if task == nil {
		return model.ReviewFindingProvenance{}, false
	}
	key := reviewFindingFingerprint(finding)
	for _, receipt := range task.ReviewFindingProvenance {
		if receipt.Finding == key && receipt.SourceTask == task.ID && receipt.Base == task.BaseSHA && receipt.Head == task.HeadSHA && reviewReceiptEvidenceMatches(task, receipt, effective) {
			return receipt, true
		}
	}
	return model.ReviewFindingProvenance{}, false
}

func reviewReceiptEvidenceMatches(task *model.Task, receipt model.ReviewFindingProvenance, effective config.Effective) bool {
	if task == nil || receipt.SourceTask != task.ID || receipt.Config != effective.Hash || receipt.Rules != roles.Hash() || task.Evidence == nil {
		return false
	}
	evidence := task.Evidence
	return evidence.Base == receipt.Base && evidence.Head == receipt.Head && evidence.Config == receipt.Config && evidence.Rules == receipt.Rules &&
		slices.Contains(evidence.ReviewRoster, receipt.Role) && evidence.Reviews[receipt.Role] != ""
}

// repairFirstRecovery selects exactly one unchanged-head route from an
// attributable review finding to the ordinary FIX lane. The receipt proves
// where a controller retained the finding; it is never an approval and all
// source changes return to full native verification and review.
func repairFirstRecovery(task *model.Task, effective config.Effective) *model.RepairFirstRecovery {
	if task == nil || task.State != model.SyncRequired || task.RepairFirst != nil || task.ReviewFindingReceiptOverflow || len(task.Findings) == 0 {
		return nil
	}
	keys := make([]string, 0, len(task.Findings))
	concrete := false
	for _, finding := range task.Findings {
		if finding.Severity != "critical" && finding.Severity != "high" {
			continue
		}
		if finding.Relevance != model.FindingChanged && finding.Relevance != model.FindingCausal {
			return nil
		}
		receipt, ok := receiptForFinding(task, finding, effective)
		if !ok || receipt.Base != task.BaseSHA || receipt.Head != task.HeadSHA {
			return nil
		}
		if receipt.ControllerSummary {
			// retry adds this marker itself from the durable exact-head review
			// summary. A provider finding with no source location never receives
			// this receipt and remains on the ordinary verification route.
			if finding.Location != "" || finding.Category != receipt.Role || finding.Role != receipt.Role || !hasAttributedConcreteReviewFinding(task, task.Evidence, receipt.Role, effective) {
				return nil
			}
		} else if !findingInTaskScope(task, finding) {
			return nil
		} else {
			concrete = true
		}
		keys = append(keys, receipt.Finding)
	}
	if len(keys) == 0 || !concrete {
		return nil
	}
	sort.Strings(keys)
	return &model.RepairFirstRecovery{Base: task.BaseSHA, Head: task.HeadSHA, Config: effective.Hash, Rules: roles.Hash(), Findings: slices.Compact(keys)}
}
