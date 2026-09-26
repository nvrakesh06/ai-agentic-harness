package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/config"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/nvrakesh06/ai-agentic-harness/internal/provider"
)

func TestProviderAdmissionSchemaScopeIgnoresPolicyAndModelProvenance(t *testing.T) {
	s := model.NewSnapshot("project123")
	schema := fmt.Sprintf("%064x", 41)
	hold := model.ProviderAdmissionHold{
		Provider: "codex", Class: model.ProviderAdmissionRequestRejected, Rejection: model.ProviderRejectionInvalidJSONSchema, SchemaSHA256: schema,
		OriginPolicy: fmt.Sprintf("%064x", 42), OriginRules: fmt.Sprintf("%064x", 43), OriginModel: "gpt-6-sol",
	}
	key, err := hold.ScopeKey()
	if err != nil {
		t.Fatal(err)
	}
	s.ProviderAdmissionHolds[key] = hold
	effective := config.Effective{Project: config.Project{Provider: "codex"}, Hash: fmt.Sprintf("%064x", 99)}
	if current, held := providerAdmissionHoldForSchema(s, effective, schema); !held || current.OriginModel != "gpt-6-sol" {
		t.Fatalf("same schema was not held: %#v held=%t", current, held)
	}
	effective.Hash = fmt.Sprintf("%064x", 100)
	effective.Project.Models = map[string]string{"implementer": "gpt-5.6-terra"}
	if _, held := providerAdmissionHoldForSchema(s, effective, schema); !held {
		t.Fatal("policy/model provenance unexpectedly reopened a schema hold")
	}
	if _, held := providerAdmissionHoldForSchema(s, effective, fmt.Sprintf("%064x", 44)); held {
		t.Fatal("material schema digest change did not reopen admission")
	}
}

func TestRecordProviderAdmissionHoldUsesReservedSaturation(t *testing.T) {
	s := model.NewSnapshot("project123")
	for i := 0; i < model.MaxExactProviderAdmissionHolds; i++ {
		hold := model.ProviderAdmissionHold{Provider: "codex", Class: model.ProviderAdmissionRequestRejected, Rejection: model.ProviderRejectionInvalidJSONSchema, SchemaSHA256: fmt.Sprintf("%064x", i+1), OriginPolicy: fmt.Sprintf("%064x", 1), OriginRules: fmt.Sprintf("%064x", 2), OriginModel: "normal"}
		if _, err := recordProviderAdmissionHold(s, hold); err != nil {
			t.Fatal(err)
		}
	}
	seventh := model.ProviderAdmissionHold{Provider: "claude-code", Class: model.ProviderAdmissionRequestRejected, Rejection: model.ProviderRejectionInvalidJSONSchema, SchemaSHA256: fmt.Sprintf("%064x", 99), OriginPolicy: fmt.Sprintf("%064x", 1), OriginRules: fmt.Sprintf("%064x", 2), OriginModel: "normal"}
	recorded, err := recordProviderAdmissionHold(s, seventh)
	if err != nil || recorded.Class != model.ProviderAdmissionSaturated || len(s.ProviderAdmissionHolds) != model.MaxExactProviderAdmissionHolds+1 {
		t.Fatalf("seventh exact hold = %#v len=%d err=%v", recorded, len(s.ProviderAdmissionHolds), err)
	}
}

func TestProviderAdmissionSaturationRetainsTypedAuthenticationCause(t *testing.T) {
	err := &providerAdmissionHeldError{
		hold:  model.ProviderAdmissionHold{Provider: "codex", Class: model.ProviderAdmissionSaturated},
		cause: &provider.InvocationError{Cause: errors.New("fixture authentication failure"), Failure: provider.FailureAuthentication},
	}
	if !provider.IsAuthenticationFailure(err) {
		t.Fatalf("saturation hold lost the authoritative authentication cause: %v", err)
	}
}

func TestProviderAdmissionRetryRequiresExactCurrentScope(t *testing.T) {
	s := model.NewSnapshot("project123")
	schema := provider.SchemaSHA256()
	hold := model.ProviderAdmissionHold{Provider: "codex", Class: model.ProviderAdmissionRequestRejected, Rejection: model.ProviderRejectionInvalidJSONSchema, SchemaSHA256: schema, OriginPolicy: fmt.Sprintf("%064x", 1), OriginRules: fmt.Sprintf("%064x", 2), OriginModel: "normal"}
	key, err := hold.ScopeKey()
	if err != nil {
		t.Fatal(err)
	}
	s.ProviderAdmissionHolds[key] = hold
	effective := config.Effective{Project: config.Project{Provider: "codex"}, Hash: fmt.Sprintf("%064x", 3)}
	if got, err := providerAdmissionRetryTarget(s, effective, key); err != nil || got != hold {
		t.Fatalf("exact active retry target = %#v, %v", got, err)
	}
	// Origin policy/model only explain the rejected request. They cannot be
	// used to release the same schema identity.
	effective.Hash = fmt.Sprintf("%064x", 4)
	effective.Project.Models = map[string]string{"reviewer": "different"}
	if _, err := providerAdmissionRetryTarget(s, effective, key); err != nil {
		t.Fatalf("policy/model provenance unexpectedly released retry target: %v", err)
	}
	if _, err := providerAdmissionRetryTarget(s, effective, "missing"); err == nil {
		t.Fatal("unknown retry target accepted")
	}
	// A corrected schema naturally makes this old hold inactive, without
	// deleting its historical record.
	if _, err := providerAdmissionRetryTarget(s, effective, key); err != nil {
		t.Fatal(err)
	}
	if _, held := providerAdmissionHoldForSchema(s, effective, fmt.Sprintf("%064x", 52)); held {
		t.Fatal("material schema repair did not reopen admission")
	}
}
