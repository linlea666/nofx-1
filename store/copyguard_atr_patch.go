package store

import "fmt"

// Explicit edits are distinct from whole-form values sent by old clients.
type CopyGuardATRProfilePatch struct {
	TriggerPriceType    *string `json:"trigger_price_type,omitempty"`
	ReentryEnabled      *bool   `json:"reentry_enabled,omitempty"`
	ReentryDecisionMode *string `json:"reentry_decision_mode,omitempty"`
	MaxReentries        *int    `json:"max_reentries,omitempty"`
}

func ApplyCopyGuardATRProfilePatch(c *CopyTradeConfig, p *CopyGuardATRProfilePatch) error {
	if p == nil {
		return nil
	}
	if c == nil {
		return fmt.Errorf("nil Copy Guard configuration")
	}
	if c.RiskProtectionMode == RiskProtectionModePositionMarginPct {
		return fmt.Errorf("ATR profile edits require ATR mode")
	}
	if p.TriggerPriceType != nil {
		c.RiskTriggerPriceType = *p.TriggerPriceType
	}
	if p.ReentryEnabled != nil {
		c.RiskReentryEnabled = *p.ReentryEnabled
	}
	if p.ReentryDecisionMode != nil {
		c.RiskReentryDecisionMode = *p.ReentryDecisionMode
	}
	if p.MaxReentries != nil {
		c.RiskMaxReentries = *p.MaxReentries
	}
	c.RiskManualReentryEnabled = false
	c.RiskATRProfile = copyGuardATRProfileFromConfig(c)
	return nil
}
