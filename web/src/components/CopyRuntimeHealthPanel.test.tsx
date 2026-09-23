import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import { SWRConfig } from 'swr'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { api } from '../lib/api'
import type {
  CopyExecutionHealthIssue,
  CopyRuntimeIssue,
  CopyTraderRuntimeHealth,
} from '../types'
import { CopyRuntimeHealthPanel } from './CopyRuntimeHealthPanel'

vi.mock('../lib/api', () => ({ api: { getCopyRuntimeHealth: vi.fn() } }))

function trader(name: string): CopyTraderRuntimeHealth {
  return {
    trader_id: name,
    trader_name: name,
    running: true,
    source: null,
    execution_issues: [],
    runtime_issues: [],
    settlement_issues: [],
  }
}

function mount(traderID?: string) {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <CopyRuntimeHealthPanel traderID={traderID} />
    </SWRConfig>
  )
}

beforeEach(() => {
  vi.mocked(api.getCopyRuntimeHealth).mockReset()
})
afterEach(cleanup)

it('keeps missing source data and historical fees separate from execution and other traders', async () => {
  const one = trader('华创国际')
  one.settlement_issues = [
    {
      exchange_id: 'account',
      identity: 'zec-cycle',
      position_id: 'pos',
      symbol: 'ZECUSDT',
      side: 'long',
      margin_mode: 'isolated',
      opened_ms: 1790022269524,
      stage: 'LIFECYCLE_PROOF',
      reason_code: 'OPENING_FILLS_INCOMPLETE',
      detail: '首单成交待核实',
      status: 'PENDING',
      attempts: 3,
      first_observed_at: '2026-09-24 00:10:00',
      last_observed_at: '2026-09-24 00:15:00',
    },
  ]
  const two = trader('另一个交易员')
  two.source = {
    last_success_at: new Date(Date.now() - 1000).toISOString(),
    last_failure_at: null,
    last_error: '',
  }
  vi.mocked(api.getCopyRuntimeHealth).mockResolvedValue({ traders: [one, two] })
  mount()
  const first = await screen.findByRole('article', { name: '华创国际运行状态' })
  expect(within(first).getByText('未取得完整快照')).toBeInTheDocument()
  expect(within(first).getByText('执行待处理 · 0')).toBeInTheDocument()
  expect(within(first).getByText('费用待核实 · 1')).toBeInTheDocument()
  expect(within(first).getByText('ZECUSDT · 多头 · 逐仓')).toBeInTheDocument()
  expect(
    within(first).getByText('历史费用待核实不阻止新交易。')
  ).toBeInTheDocument()
  const second = screen.getByRole('article', { name: '另一个交易员运行状态' })
  expect(within(second).getByText('已取得近期完整快照')).toBeInTheDocument()
  expect(within(second).getByText('执行待处理 · 0')).toBeInTheDocument()
  expect(within(second).queryByText(/首单成交/)).toBeNull()
})

it('classifies generic runtime issues by area and merges a matching execution observation', async () => {
  const one = trader('华创国际')
  const issue: CopyRuntimeIssue = {
    trader_id: one.trader_id,
    area: 'execution',
    resource_id: 'intent:12',
    leader_pos_id: 'leader-pos',
    symbol: 'ETHUSDT',
    side: 'long',
    code: 'WAIT',
    detail: '源修订待收尾',
    first_seen: '2026-09-23T13:11:50Z',
    last_seen: '2026-09-23T13:12:00Z',
  }
  one.runtime_issues = [
    issue,
    {
      ...issue,
      area: 'source',
      resource_id: 'snapshot',
      detail: '源周期身份待核实',
    },
    {
      ...issue,
      area: 'protection',
      resource_id: 'guard',
      detail: '旧保护单清理待核实',
      first_seen: '0001-01-01T00:00:00Z',
    },
  ]
  const execution: CopyExecutionHealthIssue = {
    intent_id: 12,
    trader_id: one.trader_id,
    leader_pos_id: 'leader-pos',
    action: 'open_long',
    symbol: 'ETHUSDT',
    side: 'long',
    margin_mode: 'cross',
    revision: 10,
    status: 'SKIPPED',
    reason: 'POSITION_CUSTODY_RELEASED',
    submitted: false,
    unresolved_attempts: 0,
    effect: 'UNSUBMITTED',
    mapping_status: 'active',
    mapping_revision: 9,
    created_at: '2026-09-18 10:00:00',
    updated_at: '2026-09-18 10:01:00',
    issue_reason: '源修订尚未确认，后续动作等待',
  }
  one.execution_issues = [execution]
  vi.mocked(api.getCopyRuntimeHealth).mockResolvedValue({ traders: [one] })
  mount()
  const source = await screen.findByRole('region', { name: '华创国际源快照' })
  expect(within(source).getByText('源周期身份待核实')).toBeInTheDocument()
  const executionSection = screen.getByRole('region', {
    name: '华创国际执行待处理',
  })
  expect(
    within(executionSection).getByText('执行待处理 · 1')
  ).toBeInTheDocument()
  expect(
    within(executionSection).getByText(/影响范围：本交易员该合约、该方向/)
  ).toBeInTheDocument()
  expect(within(executionSection).getByText(/^首次等待：/)).toBeInTheDocument()
  const protection = screen.getByRole('region', { name: '华创国际保护待办' })
  expect(within(protection).getByText('保护待办 · 1')).toBeInTheDocument()
  expect(within(protection).getByText('旧保护单清理待核实')).toBeInTheDocument()
  expect(within(protection).getByText(/^首次等待：未记录/)).toBeInTheDocument()
  expect(within(protection).queryByText('源周期身份待核实')).toBeNull()
})

it('exposes request failures and explicit retry instead of an empty healthy result', async () => {
  vi.mocked(api.getCopyRuntimeHealth)
    .mockRejectedValueOnce(new Error('Request timeout'))
    .mockResolvedValue({ traders: [] })
  mount()
  expect(await screen.findByRole('alert')).toHaveTextContent('运行状态读取失败')
  expect(screen.queryByText('当前没有运行中的跟单交易员。')).toBeNull()
  fireEvent.click(screen.getByRole('button', { name: '刷新状态' }))
  expect(
    await screen.findByText('当前没有运行中的跟单交易员。')
  ).toBeInTheDocument()
  await waitFor(() => expect(api.getCopyRuntimeHealth).toHaveBeenCalledTimes(2))
})

it('does not show cached healthy status after a refresh failure', async () => {
  const one = trader('测试交易员')
  one.source = {
    last_success_at: new Date().toISOString(),
    last_failure_at: null,
    last_error: '',
  }
  vi.mocked(api.getCopyRuntimeHealth)
    .mockResolvedValueOnce({ traders: [one] })
    .mockRejectedValue(new Error('Network error'))
  mount()
  expect(await screen.findByText('已取得近期完整快照')).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: '刷新状态' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('当前状态无法确认')
  expect(screen.queryByText('已取得近期完整快照')).toBeNull()
})

it('shows stale source snapshots and keeps stopped history out of live failure wording', async () => {
  const one = trader('已停止交易员')
  one.running = false
  one.source = {
    last_success_at: new Date(Date.now() - 120000).toISOString(),
    last_failure_at: null,
    last_error: '',
  }
  vi.mocked(api.getCopyRuntimeHealth).mockResolvedValue({ traders: [one] })
  mount(one.trader_id)
  expect(await screen.findByText('已停止 · 历史待办')).toBeInTheDocument()
  expect(screen.getByText('快照超过 60 秒未更新')).toBeInTheDocument()
  expect(screen.getByText(/以下为历史状态，不计作当前漏单/)).toBeInTheDocument()
  expect(api.getCopyRuntimeHealth).toHaveBeenCalledWith(one.trader_id)
})

it('does not pretend intent creation is the first observed wait', async () => {
  const one = trader('历史意图')
  one.execution_issues = [
    {
      intent_id: 9,
      trader_id: one.trader_id,
      leader_pos_id: 'pos',
      action: 'close_short',
      symbol: 'BTCUSDT',
      side: 'short',
      margin_mode: 'cross',
      revision: 3,
      mapping_revision: 20,
      mapping_status: 'closed',
      status: 'RECONCILING',
      effect: 'BOOKED_FILL',
      reason: 'REVISION_CONFLICT',
      issue_reason: '历史成交收尾待完成',
      submitted: true,
      unresolved_attempts: 0,
      created_at: '2026-09-18 01:02:03',
      updated_at: '2026-09-24 01:02:03',
    },
  ]
  vi.mocked(api.getCopyRuntimeHealth).mockResolvedValue({ traders: [one] })
  mount()
  expect(
    await screen.findByText(/首次等待未记录；指令创建/)
  ).toBeInTheDocument()
  expect(screen.getByText('成交已入账，等待源状态收尾')).toBeInTheDocument()
})
