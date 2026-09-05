import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { CopyGuardPage } from './CopyGuardPage'

vi.mock('swr', () => ({
  default: (key: string | null) => ({
    data: key?.startsWith('copy-guard-cycles-')
      ? [cycle]
      : key === 'copy-guard-cycle-1'
        ? detail
        : undefined,
    mutate: vi.fn(),
  }),
}))
const cycle = {
  id: 1,
  trader_id: 't',
  leader_id: 'leader',
  leader_pos_id: 'p',
  symbol: 'ETHUSDT',
  side: 'long',
  margin_mode: 'cross',
  status: 'FOLLOWING',
  stop_count: 0,
  reentry_count: 0,
  actual_pnl: 0,
  baseline_pnl: 0,
  net_guard_effect: 0,
  tracking_difference: 0,
  accounting_status: 'OPEN',
  accounting_error: '',
  protection_status: 'DEGRADED',
  protection_coverage: 1,
  protection_retries: 1,
  opened_at: '2026-09-05T01:00:00Z',
  policy_snapshot: 'SECRET_MUST_NOT_RENDER',
}
const audit = {
  configured_margin_loss_pct: 0.8,
  stop_anchor_entry_price: 100,
  stop_anchor_leverage: 10,
  stop_anchor_initial_margin: 10,
  stop_anchor_theoretical_risk_usd: 8,
  raw_formula_stop_price: 92,
  tick_aligned_stop_price: 92,
  effective_stop_price: 92,
  desired_stop_price: 95,
  current_entry_price: 100,
  current_quantity: 2,
  current_leverage: 10,
  current_margin: 20,
  current_stop_risk_usd: 16,
  current_margin_loss_pct: 0.8,
  current_account_loss_pct: 0.016,
  equivalent_price_move_pct: 0.08,
  distance_atr: 0,
  liquidation_clamped: true,
  governed_by: 'position_margin_liquidation_clamp',
  trigger_type: 'mark',
  hosted_order_id: 'old-order',
  coverage_mode: 'CLOSE_ALL',
  coverage_ratio: 1,
  protection_status: 'DEGRADED',
  data_quality: 'PARTIAL',
  unscorable_reason: '旧保护仍有效，新价等待重试',
  protection_verified_at: '2026-09-05T01:00:00Z',
}
const detail = {
  cycle,
  attempts: [],
  events: [],
  policy: {
    snapshot_schema_version: 2,
    policy_version: 4,
    protection_mode: 'position_margin_pct',
    position_margin_stop_pct: 0.8,
    trigger_price_type: 'mark',
    reentry_enabled: false,
    data_quality: 'VERIFIED',
  },
  position_margin_audit: audit,
  shadow_evaluations: [{ policy: 'FIRST_ENTRY_POSITION_MARGIN_80' }],
}

afterEach(cleanup)
it('renders backend risk facts, distinguishes desired versus hosted stop, and hides raw snapshots and shadow history', () => {
  render(<CopyGuardPage />)
  fireEvent.click(screen.getByText('ETHUSDT'))
  expect(screen.getByText('固定仓位止损审计')).toBeInTheDocument()
  expect(screen.getByText('策略要求价（不等于已挂单）')).toBeInTheDocument()
  expect(screen.getByText('20.00 / 16.00 USDT')).toBeInTheDocument()
  expect(screen.getByText(/old-order/)).toBeInTheDocument()
  expect(screen.getByText('保护单核验时间')).toBeInTheDocument()
  expect(screen.getByText(/旧保护仍有效/)).toBeInTheDocument()
  expect(screen.queryByText(/SECRET_MUST_NOT_RENDER/)).toBeNull()
  expect(
    screen.queryByText(/FIRST_ENTRY_POSITION_MARGIN_80|影子评测|晋级/)
  ).toBeNull()
})
