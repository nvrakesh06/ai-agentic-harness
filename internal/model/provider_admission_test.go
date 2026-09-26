package model

import (
	"encoding/json"
	"fmt"
	"testing"
)

func providerAdmissionHold(provider, class, rejection, schema string) ProviderAdmissionHold {
	return ProviderAdmissionHold{
		Provider: provider, Class: class, Rejection: rejection, SchemaSHA256: schema,
		OriginPolicy: fmt.Sprintf("%064x", 1), OriginRules: fmt.Sprintf("%064x", 2), OriginModel: "gpt-6-sol",
	}
}

func addProviderAdmissionHold(t *testing.T, s *Snapshot, hold ProviderAdmissionHold) {
	t.Helper()
	key, err := hold.ScopeKey()
	if err != nil {
		t.Fatal(err)
	}
	s.ProviderAdmissionHolds[key] = hold
}

func TestProviderAdmissionHoldsMigrateAndRoundTrip(t *testing.T) {
	legacy := NewSnapshot("project123")
	legacy.Schema = 11
	addProviderAdmissionHold(t, legacy, providerAdmissionHold("codex", ProviderAdmissionRequestRejected, ProviderRejectionInvalidJSONSchema, fmt.Sprintf("%064x", 3)))
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	recovered, migrated, err := Decode(b)
	if err != nil || !migrated || recovered.Schema != StateSchema || len(recovered.ProviderAdmissionHolds) != 0 {
		t.Fatalf("schema-11 hold migration = %#v migrated=%t err=%v", recovered.ProviderAdmissionHolds, migrated, err)
	}

	current := NewSnapshot("project123")
	hold := providerAdmissionHold("codex", ProviderAdmissionRequestRejected, ProviderRejectionInvalidJSONSchema, fmt.Sprintf("%064x", 4))
	addProviderAdmissionHold(t, current, hold)
	b, err = json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, migrated, err := Decode(b)
	if err != nil || migrated || len(roundTrip.ProviderAdmissionHolds) != 1 {
		t.Fatalf("current hold round-trip = %#v migrated=%t err=%v", roundTrip.ProviderAdmissionHolds, migrated, err)
	}
	if clone := Clone(roundTrip); len(clone.ProviderAdmissionHolds) != 1 {
		t.Fatalf("clone lost provider admission hold: %#v", clone.ProviderAdmissionHolds)
	}
}

func TestProviderAdmissionHoldValidationAndReservedSaturationCapacity(t *testing.T) {
	s := NewSnapshot("project123")
	for i := 0; i < MaxExactProviderAdmissionHolds; i++ {
		addProviderAdmissionHold(t, s, providerAdmissionHold("codex", ProviderAdmissionRequestRejected, ProviderRejectionInvalidJSONSchema, fmt.Sprintf("%064x", i+10)))
	}
	addProviderAdmissionHold(t, s, providerAdmissionHold("codex", ProviderAdmissionSaturated, "", ""))
	addProviderAdmissionHold(t, s, providerAdmissionHold("claude-code", ProviderAdmissionSaturated, "", ""))
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, migrated, err := Decode(b); err != nil || migrated {
		t.Fatalf("six exact plus two reserved saturation holds = migrated=%t err=%v", migrated, err)
	}

	extra := providerAdmissionHold("claude-code", ProviderAdmissionRequestRejected, ProviderRejectionInvalidJSONSchema, fmt.Sprintf("%064x", 99))
	addProviderAdmissionHold(t, s, extra)
	b, err = json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(b); err == nil {
		t.Fatal("seventh exact provider admission hold accepted")
	}

	key, _ := ProviderAdmissionHoldKey("codex", ProviderAdmissionAuthentication, "", "")
	s = NewSnapshot("project123")
	s.ProviderAdmissionHolds[key] = providerAdmissionHold("codex", ProviderAdmissionAuthentication, "", "")
	s.ProviderAdmissionHolds["wrong"] = s.ProviderAdmissionHolds[key]
	b, err = json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(b); err == nil {
		t.Fatal("mismatched provider admission hold key accepted")
	}
}
