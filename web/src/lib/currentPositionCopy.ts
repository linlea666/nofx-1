import type {
  CopyTradeConfig,
  CopyTradeProvider,
  CurrentPositionCopySelection,
} from '../types'

// getRandomValues also works on the HTTP self-hosted deployments supported by
// this project; randomUUID is only available in a secure browser context.
export function newCurrentPositionCopyRequestId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  const hex = Array.from(bytes, (byte) =>
    byte.toString(16).padStart(2, '0')
  ).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

// Keep in sync with store.CurrentPositionCopySupported on the backend.
export function currentPositionCopySupported(
  provider: CopyTradeProvider
): boolean {
  return provider === 'okx' || provider === 'followcoin'
}

// Restores the selection/price options of a request that has not run yet, so
// re-saving the trader keeps what the operator chose. Finished or cancelled
// requests fall back to the defaults (filter on, not worse than leader entry,
// positions up to 1% worse may be picked by hand).
export const DEFAULT_MANUAL_MAX_DISADVANTAGE_PCT = 1

export function pendingCurrentCopyOptions(cfg: CopyTradeConfig): {
  copy_current_positions_selection?: CurrentPositionCopySelection[]
  copy_current_price_filter_enabled: boolean
  copy_current_min_advantage_pct: number
  copy_current_manual_max_disadvantage_pct: number
} {
  const pending = cfg.current_position_copy
  if (
    !cfg.copy_current_positions_once ||
    !pending ||
    pending.status !== 'PENDING' ||
    pending.request_id !== cfg.copy_current_positions_request_id
  ) {
    return {
      copy_current_positions_selection: undefined,
      copy_current_price_filter_enabled: true,
      copy_current_min_advantage_pct: 0,
      copy_current_manual_max_disadvantage_pct:
        DEFAULT_MANUAL_MAX_DISADVANTAGE_PCT,
    }
  }
  return {
    copy_current_positions_selection:
      pending.selection_mode === 'SELECTED' ? pending.selection : undefined,
    copy_current_price_filter_enabled: pending.price_filter_enabled ?? false,
    copy_current_min_advantage_pct: pending.min_advantage_pct ?? 0,
    copy_current_manual_max_disadvantage_pct:
      pending.manual_max_disadvantage_pct ?? 0,
  }
}

// Spells out why a row missed the price rule, so the operator sees the gap and
// which setting to change instead of a bare "价格无优势".
// Keep in sync with store currentCopyManualMaxDisadvantagePctCeiling.
export const MANUAL_MAX_DISADVANTAGE_CEILING_PCT = 20
export function priceSkipNote(
  advantage: number,
  minAdvantagePct: number,
  manualMaxDisadvantagePct: number
): string {
  const adv = `${advantage >= 0 ? '+' : ''}${advantage.toFixed(2)}%`
  if (manualMaxDisadvantagePct <= 0)
    return `优势 ${adv} 低于最低优势 ${minAdvantagePct}%（手选已关闭：「手选最大劣势」设为大于 0 可放宽，或关闭价格过滤）`
  const gap = -advantage
  if (gap <= 0)
    return `优势 ${adv} 低于最低优势 ${minAdvantagePct}%，本次不复制（下次可再选）`
  const needed = Math.ceil(gap * 10) / 10
  if (needed > MANUAL_MAX_DISADVANTAGE_CEILING_PCT)
    return `劣势 ${gap.toFixed(2)}% 超过手选上限可设的最大值 ${MANUAL_MAX_DISADVANTAGE_CEILING_PCT}%，本次不复制（可关闭价格过滤）`
  return `劣势 ${gap.toFixed(2)}% 超过手选上限 ${manualMaxDisadvantagePct}%：将「手选最大劣势」调到 ≥${needed}% 后可手动勾选，或关闭价格过滤`
}
