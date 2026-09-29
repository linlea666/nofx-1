import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CurrentPositionCopyControl } from './CurrentPositionCopyControl'
import { httpClient } from '../lib/httpClient'
import { newCurrentPositionCopyRequestId } from '../lib/currentPositionCopy'
import type { CurrentPositionCopyPreview } from '../types'

vi.mock('../lib/httpClient', () => ({ httpClient: { post: vi.fn() } }))
const preview: CurrentPositionCopyPreview = {
  snapshot_at: '2026-09-29T10:00:00Z',
  leader_equity: 1000,
  follower_equity: 100,
  positions: [
    {
      id: 1,
      leader_pos_id: 'p',
      symbol: 'ETHUSDT',
      side: 'long',
      margin_mode: 'cross',
      opened_ms: 1790000000000,
      source_size: 10,
      leader_entry_price: 100,
      reference_price: 110,
      estimated_notional: 100,
      leverage: 10,
      status: 'READY',
      reason: '',
      intent_id: 0,
      filled_quantity: 0,
    },
  ],
}
const props = {
  enabled: false,
  disabled: false,
  exchangeId: 'account',
  leaderId: 'leader',
  ratio: 1,
  onChange: vi.fn(),
}
beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(httpClient.post).mockResolvedValue({ success: true, data: preview })
})
afterEach(cleanup)

describe('one-time current-position copy', () => {
  it('stays off and does not request or submit anything until explicitly enabled', async () => {
    render(<CurrentPositionCopyControl {...props} />)
    const checkbox = screen.getByRole('checkbox')
    expect(checkbox).not.toBeChecked()
    expect(httpClient.post).not.toHaveBeenCalled()
    fireEvent.click(checkbox)
    expect(props.onChange).toHaveBeenCalledWith(true)
    expect(httpClient.post).not.toHaveBeenCalled()
  })
  it('only reads a preview and keeps worse prices eligible', async () => {
    render(<CurrentPositionCopyControl {...props} enabled traderId="trader" />)
    expect(await screen.findByText(/-10\.00%/)).toBeVisible()
    expect(screen.getByText('符合复制条件')).toBeVisible()
    expect(httpClient.post).toHaveBeenCalledOnce()
    expect(httpClient.post).toHaveBeenCalledWith(
      '/api/copytrade/current-positions/preview',
      {
        trader_id: 'trader',
        exchange_id: 'account',
        provider_type: 'okx',
        leader_id: 'leader',
        copy_ratio: 1,
      }
    )
    expect(screen.getByText(/保存仅登记请求/)).toBeVisible()
  })
  it('discards a late preview when the request is disabled', async () => {
    let resolve!: (value: Awaited<ReturnType<typeof httpClient.post>>) => void
    vi.mocked(httpClient.post).mockReturnValue(
      new Promise((done) => {
        resolve = done
      })
    )
    const { rerender } = render(
      <CurrentPositionCopyControl {...props} enabled />
    )
    await waitFor(() => expect(httpClient.post).toHaveBeenCalledOnce())
    rerender(<CurrentPositionCopyControl {...props} />)
    resolve({ success: true, data: preview })
    expect(screen.queryByText('ETHUSDT')).not.toBeInTheDocument()
  })
  it('shows a reason and a useful empty result without enabling trading', async () => {
    vi.mocked(httpClient.post).mockResolvedValueOnce({
      success: false,
      message: '账户无权限',
    })
    const { rerender } = render(
      <CurrentPositionCopyControl {...props} enabled />
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('账户无权限')
    vi.mocked(httpClient.post).mockResolvedValueOnce({
      success: true,
      data: { ...preview, positions: [] },
    })
    rerender(
      <CurrentPositionCopyControl {...props} enabled leaderId="another" />
    )
    expect(await screen.findByText(/当前无可复制仓位/)).toBeVisible()
    expect(props.onChange).not.toHaveBeenCalled()
  })
  it('disables registration while the trader is running', () => {
    render(<CurrentPositionCopyControl {...props} disabled />)
    expect(screen.getByRole('checkbox')).toBeDisabled()
  })
  it('generates distinct UUID v4 request identities without secure-context randomUUID', () => {
    const a = newCurrentPositionCopyRequestId(),
      b = newCurrentPositionCopyRequestId()
    expect(a).toMatch(
      /^[a-f\d]{8}-[a-f\d]{4}-4[a-f\d]{3}-[89ab][a-f\d]{3}-[a-f\d]{12}$/
    )
    expect(a).not.toBe(b)
  })
})
