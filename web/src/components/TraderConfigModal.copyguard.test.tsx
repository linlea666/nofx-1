import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { TraderConfigModal } from './TraderConfigModal'
import { httpClient } from '../lib/httpClient'
import type { TraderConfigData } from '../types'

vi.mock('../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: 'zh' }),
}))
vi.mock('../lib/httpClient', () => ({ httpClient: { get: vi.fn() } }))
vi.mock('sonner', () => ({
  toast: { promise: (p: Promise<void>) => p, error: vi.fn() },
}))

let supported = true
const config = {
  provider_type: 'okx',
  leader_id: 'leader',
  copy_ratio: 1,
  risk_stop_loss_enabled: true,
  risk_policy_version: 4,
  risk_protection_mode: 'atr_structure',
  risk_position_margin_stop_pct: 0.8,
  risk_trigger_price_type: 'index',
  risk_reentry_enabled: true,
  risk_reentry_decision_mode: 'ai_guarded',
  risk_max_reentries: 2,
}

function mount() {
  const onSave = vi.fn().mockResolvedValue(undefined)
  render(
    <TraderConfigModal
      isOpen
      isEditMode
      onClose={vi.fn()}
      onSave={onSave}
      availableModels={[]}
      availableExchanges={[]}
      traderData={
        {
          trader_id: 't',
          trader_name: 'Trader',
          decision_mode: 'copy_trade',
          exchange_id: 'e',
          ai_model: 'm',
          strategy_id: 's',
        } as TraderConfigData
      }
    />
  )
  return onSave
}

beforeEach(() => {
  supported = true
  vi.spyOn(window, 'alert').mockImplementation(() => {})
  vi.mocked(httpClient.get).mockImplementation(
    async (path) =>
      ({
        success: true,
        data: path.includes('/config/')
          ? { config }
          : path.includes('/risk/defaults')
            ? {
                copy_guard_capabilities: {
                  fixed_initial_margin: supported,
                  live_hardening_version: supported ? 1 : 0,
                  shadow_runtime_enabled: false,
                },
              }
            : { strategies: [] },
      }) as Awaited<ReturnType<typeof httpClient.get>>
  )
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

describe('Copy Guard trader configuration', () => {
  it.each([1, 80, 99])(
    'saves valid %s%% as a fraction and forbids fixed reentry even with the new-position switch off',
    async (pct) => {
      const onSave = mount()
      const mode = await screen.findByLabelText('止损模式')
      await waitFor(() =>
        expect(
          screen.getByRole('option', { name: '首仓固化保证金比例止损' })
        ).not.toBeDisabled()
      )
      fireEvent.change(mode, { target: { value: 'position_margin_pct' } })
      fireEvent.change(screen.getByLabelText(/首仓保证金止损比例/), {
        target: { value: String(pct) },
      })
      fireEvent.click(screen.getByRole('button', { name: '启用账户保护止损' }))
      fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
      await waitFor(() => expect(onSave).toHaveBeenCalledOnce())
      expect(onSave.mock.calls[0][0].copy_config).toMatchObject({
        risk_stop_loss_enabled: false,
        risk_protection_mode: 'position_margin_pct',
        risk_position_margin_stop_pct: pct / 100,
        risk_reentry_enabled: false,
        risk_reentry_decision_mode: 'disabled',
        risk_manual_reentry_enabled: false,
        risk_max_reentries: 0,
        risk_trigger_price_type: 'mark',
      })
    }
  )
  it('restores an unsaved ATR trigger draft across fixed mode and explains the new-cycle boundary', async () => {
    mount()
    const mode = await screen.findByLabelText('止损模式')
    const trigger = screen.getByLabelText('触发价格')
    expect((trigger as HTMLSelectElement).value).toBe('index')
    fireEvent.change(trigger, { target: { value: 'last' } })
    await waitFor(() =>
      expect(
        screen.getByRole('option', { name: '首仓固化保证金比例止损' })
      ).not.toBeDisabled()
    )
    fireEvent.change(mode, { target: { value: 'position_margin_pct' } })
    expect(screen.getByLabelText(/首仓保证金止损比例/)).toHaveValue(80)
    expect(screen.queryByLabelText('触发价格')).toBeNull()
    expect(screen.getByText(/只影响下一次全新开仓/)).toBeInTheDocument()
    fireEvent.change(mode, { target: { value: 'atr_structure' } })
    expect(screen.getByLabelText('触发价格')).toHaveValue('last')
  })

  it('disables fixed mode on an older backend without disabling ATR', async () => {
    supported = false
    mount()
    await screen.findByLabelText('止损模式')
    expect(
      screen.getByRole('option', { name: '首仓固化保证金比例止损' })
    ).toBeDisabled()
    expect(screen.getByLabelText('止损模式')).not.toBeDisabled()
  })

  it.each([0, 100])(
    'rejects out-of-range percentage %s without saving',
    async (pct) => {
      const onSave = mount()
      const mode = await screen.findByLabelText('止损模式')
      await waitFor(() =>
        expect(
          screen.getByRole('option', { name: '首仓固化保证金比例止损' })
        ).not.toBeDisabled()
      )
      fireEvent.change(mode, { target: { value: 'position_margin_pct' } })
      fireEvent.change(screen.getByLabelText(/首仓保证金止损比例/), {
        target: { value: String(pct) },
      })
      fireEvent.click(screen.getByRole('button', { name: '保存修改' }))
      expect(window.alert).toHaveBeenCalled()
      expect(onSave).not.toHaveBeenCalled()
    }
  )
})
