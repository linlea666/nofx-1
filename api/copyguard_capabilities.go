package api

import (
	"fmt"
	"nofx/store"
	"runtime/debug"
)

// Read back the persisted risk whitelist. Never report a successful save when
// schema/version skew silently dropped the fixed-mode contract.
func verifyCopyGuardConfigReadback(st *store.Store, expected *store.CopyTradeConfig) error {
	actual, err := st.CopyTrade().GetByTraderID(expected.TraderID)
	if err != nil {
		return fmt.Errorf("configuration persisted but readback failed: %w", err)
	}
	want, err := store.EncodeCopyGuardPolicySnapshot(expected)
	if err != nil {
		return err
	}
	got, err := store.EncodeCopyGuardPolicySnapshot(actual)
	if err != nil {
		return err
	}
	if want != got || actual.RiskStopLossEnabled != expected.RiskStopLossEnabled {
		return fmt.Errorf("configuration persisted but risk policy readback differs; verify backend version before enabling new positions")
	}
	return nil
}

func copyGuardRuntimeCapabilities() map[string]interface{} {
	revision := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
		}
	}
	return map[string]interface{}{
		"fixed_initial_margin": true, "live_hardening_version": 1,
		"shadow_runtime_enabled": store.CopyGuardShadowRuntimeEnabled, "build_revision": revision,
	}
}
