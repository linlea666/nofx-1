package api

import (
	"testing"

	"nofx/store"
)

func TestLegacyReentrySelectionIsOneWayCompatibility(t *testing.T) {
	if err := validateLegacyReentrySelection(nil, "legacy_rule"); err == nil {
		t.Fatal("new configuration must not select legacy_rule")
	}
	if err := validateLegacyReentrySelection(&store.CopyTradeConfig{RiskReentryDecisionMode: "ai_guarded"}, "legacy_rule"); err == nil {
		t.Fatal("non-legacy configuration must not opt back into legacy_rule")
	}
	legacy := &store.CopyTradeConfig{RiskReentryDecisionMode: "legacy_rule"}
	if err := validateLegacyReentrySelection(legacy, "legacy_rule"); err != nil {
		t.Fatalf("existing legacy configuration must remain saveable during migration: %v", err)
	}
	if err := validateLegacyReentrySelection(legacy, "ai_guarded"); err != nil {
		t.Fatalf("legacy configuration must be able to migrate out: %v", err)
	}
}
