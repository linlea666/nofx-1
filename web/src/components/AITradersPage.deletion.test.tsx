import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AITradersPage } from './AITradersPage'
import { api } from '../lib/api'
import { confirmToast } from '../lib/notify'
import { toast } from 'sonner'

const fixture = vi.hoisted(() => ({
  user: { id: 'owner' },
  trader: {
    trader_id: 'old',
    trader_name: '九川',
    ai_model: 'deepseek',
    exchange: 'okx',
    exchange_id: 'account',
    is_running: false,
    lifecycle_status: 'STOPPED',
    pending_blockers: [
      { code: 'PROTECTIVE_ORDER_UNRESOLVED', status: 'mark_crossed_exit' },
    ],
  },
  mutate: vi.fn(),
}))
vi.mock('swr', () => ({
  default: () => ({
    data: [fixture.trader],
    mutate: fixture.mutate,
    isLoading: false,
  }),
}))
vi.mock('../contexts/LanguageContext', () => ({
  useLanguage: () => ({ language: 'zh' }),
}))
vi.mock('../contexts/AuthContext', () => ({
  useAuth: () => ({ user: fixture.user, token: 'test' }),
}))
vi.mock('react-router-dom', () => ({ useNavigate: () => vi.fn() }))
vi.mock('../lib/api', () => ({
  api: {
    getModelConfigs: vi.fn().mockResolvedValue([]),
    getExchangeConfigs: vi.fn().mockResolvedValue([]),
    getSupportedModels: vi.fn().mockResolvedValue([]),
    getSupportedExchanges: vi.fn().mockResolvedValue([]),
    deleteTrader: vi.fn(),
  },
}))
vi.mock('../lib/notify', () => ({ confirmToast: vi.fn() }))
vi.mock('sonner', () => ({
  toast: { loading: vi.fn(), success: vi.fn(), error: vi.fn() },
}))
vi.mock('./TraderConfigModal', () => ({ TraderConfigModal: () => null }))
vi.mock('./traders/ExchangeConfigModal', () => ({
  ExchangeConfigModal: () => null,
}))
vi.mock('./traders/BinanceGlobalCredsModal', () => ({
  BinanceGlobalCredsModal: () => null,
}))

beforeEach(() => {
  vi.clearAllMocks()
  fixture.trader.lifecycle_status = 'STOPPED'
  fixture.trader.is_running = false
  vi.mocked(confirmToast).mockResolvedValue(true)
  vi.mocked(api.deleteTrader).mockResolvedValue(undefined)
})
afterEach(cleanup)

describe('permanent trader deletion', () => {
  it('allows server verification despite old local blockers and reports release', async () => {
    render(<AITradersPage />)
    const button = screen.getByRole('button', { name: '删除交易员 九川' })
    expect(button).toBeEnabled()
    fireEvent.click(button)
    await waitFor(() => expect(api.deleteTrader).toHaveBeenCalledWith('old'))
    expect(confirmToast).toHaveBeenCalledWith(
      expect.stringContaining('不再要求补齐历史成交'),
      expect.anything()
    )
    await waitFor(() =>
      expect(toast.success).toHaveBeenCalledWith(
        '交易员已删除，账户已释放',
        expect.anything()
      )
    )
    expect(fixture.mutate).toHaveBeenCalled()
  })

  it('allows abandoned stopped work to be deleted without requiring historical reconciliation', () => {
    fixture.trader.lifecycle_status = 'STOPPING_RECONCILE_REQUIRED'
    render(<AITradersPage />)
    expect(
      screen.getByRole('button', { name: '删除交易员 九川' })
    ).toBeEnabled()
  })

  it('keeps running trader deletion disabled', () => {
    fixture.trader.lifecycle_status = 'RUNNING'
    fixture.trader.is_running = true
    render(<AITradersPage />)
    expect(
      screen.getByRole('button', { name: '删除交易员 九川' })
    ).toBeDisabled()
  })

  it('shows the actual venue blocker and does not report success', async () => {
    vi.mocked(api.deleteTrader).mockRejectedValue(
      new Error('账户仍有真实仓位：SNDKUSDT')
    )
    render(<AITradersPage />)
    fireEvent.click(screen.getByRole('button', { name: '删除交易员 九川' }))
    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(
        '账户仍有真实仓位：SNDKUSDT',
        expect.anything()
      )
    )
    expect(toast.success).not.toHaveBeenCalled()
    expect(fixture.mutate).not.toHaveBeenCalled()
  })

  it('does nothing when deletion is cancelled', async () => {
    vi.mocked(confirmToast).mockResolvedValue(false)
    render(<AITradersPage />)
    fireEvent.click(screen.getByRole('button', { name: '删除交易员 九川' }))
    await waitFor(() => expect(confirmToast).toHaveBeenCalled())
    expect(api.deleteTrader).not.toHaveBeenCalled()
  })
})
