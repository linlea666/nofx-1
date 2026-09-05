import type { CopyTradeConfig } from '../types'

export type ATRProfile = NonNullable<CopyTradeConfig['atr_profile']>
export interface CopyGuardCapabilities {
  fixed_initial_margin: boolean
  live_hardening_version: number
  build_revision: string
  shadow_runtime_enabled: boolean
}

export function fixedModeSupported(
  capabilities?: CopyGuardCapabilities
): boolean {
  return (
    capabilities?.fixed_initial_margin === true &&
    capabilities.live_hardening_version >= 1
  )
}

export function atrProfileFromForm(form: {
  risk_trigger_price_type: ATRProfile['trigger_price_type']
  risk_reentry_enabled: boolean
  risk_reentry_decision_mode: ATRProfile['reentry_decision_mode']
  risk_max_reentries: number
}): ATRProfile {
  return {
    trigger_price_type: form.risk_trigger_price_type,
    reentry_enabled: form.risk_reentry_enabled,
    reentry_decision_mode: form.risk_reentry_decision_mode,
    manual_reentry_enabled: false,
    max_reentries: form.risk_max_reentries,
  }
}
