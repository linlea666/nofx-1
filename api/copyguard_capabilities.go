package api

import (
	"fmt"
	"nofx/copytrade"
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
	// The store persists no Copy Guard policy for sources that cannot run it
	// (it deletes the policy row below v4), so the in-memory factory defaults
	// can never round-trip. Reading the row back is the whole contract here.
	if !copytrade.SupportsCopyGuard(copytrade.ProviderType(expected.ProviderType)) {
		return nil
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
