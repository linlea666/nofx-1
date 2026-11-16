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
        price_filter_enabled: false,
        min_advantage_pct: 0,
        manual_max_disadvantage_pct: 0,
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
  it('previews FollowCoin with the price rule and defaults to every ready position', async () => {
    const onOptionsChange = vi.fn()
    render(
      <CurrentPositionCopyControl
        {...props}
        enabled
        providerType="followcoin"
        priceFilterEnabled
        minAdvantagePct={0.5}
        onOptionsChange={onOptionsChange}
      />
    )
    await waitFor(() =>
      expect(onOptionsChange).toHaveBeenCalledWith({
        selection: [{ leader_pos_id: 'p', opened_ms: 1790000000000 }],
      })
    )
    expect(httpClient.post).toHaveBeenCalledWith(
      '/api/copytrade/current-positions/preview',
      expect.objectContaining({
        provider_type: 'followcoin',
        price_filter_enabled: true,
        min_advantage_pct: 0.5,
      })
    )
  })
  it('lets a slightly worse price be picked by hand but never preselects it', async () => {
    const mixed: CurrentPositionCopyPreview = {
      ...preview,
      positions: [
        preview.positions[0],
        {
          ...preview.positions[0],
          id: 2,
          leader_pos_id: 's',
          symbol: 'SUSDT',
          side: 'short',
          leader_entry_price: 100,
          reference_price: 99.8,
          price_manual_only: true,
        },
      ],
    }
    vi.mocked(httpClient.post).mockResolvedValue({ success: true, data: mixed })
    const onOptionsChange = vi.fn()
    const { rerender } = render(
      <CurrentPositionCopyControl
        {...props}
        enabled
        priceFilterEnabled
        manualMaxDisadvantagePct={1}
        onOptionsChange={onOptionsChange}
      />
    )
    await waitFor(() =>
      expect(onOptionsChange).toHaveBeenCalledWith({
        selection: [{ leader_pos_id: 'p', opened_ms: 1790000000000 }],
      })
    )
    expect(httpClient.post).toHaveBeenCalledWith(
      '/api/copytrade/current-positions/preview',
      expect.objectContaining({ manual_max_disadvantage_pct: 1 })
    )
    const selection = [{ leader_pos_id: 'p', opened_ms: 1790000000000 }]
    rerender(
      <CurrentPositionCopyControl
        {...props}
        enabled
        priceFilterEnabled
        manualMaxDisadvantagePct={1}
        selection={selection}
        onOptionsChange={onOptionsChange}
      />
    )
    expect(await screen.findByText('价格略差，可手动勾选')).toBeVisible()
    const manual = screen.getByLabelText('复制 SUSDT')
    expect(manual).toBeEnabled()
    expect(manual).not.toBeChecked()
    fireEvent.click(manual)
    expect(onOptionsChange).toHaveBeenLastCalledWith({
      selection: [
        ...selection,
        {
          leader_pos_id: 's',
          opened_ms: 1790000000000,
          accept_unfavorable: true,
        },
      ],
    })
  })
  it('keeps a saved selection and ticks a whole symbol+side group together', async () => {
    const legs: CurrentPositionCopyPreview = {
      ...preview,
      positions: [
        preview.positions[0],
        { ...preview.positions[0], id: 2, leader_pos_id: 'q' },
        {
          ...preview.positions[0],
          id: 3,
          leader_pos_id: 'r',
          symbol: 'BTCUSDT',
          status: 'SKIPPED',
          reason: 'PRICE_NOT_FAVORABLE',
        },
      ],
    }
    vi.mocked(httpClient.post).mockResolvedValue({ success: true, data: legs })
    const onOptionsChange = vi.fn()
    render(
      <CurrentPositionCopyControl
        {...props}
        enabled
        selection={[]}
        onOptionsChange={onOptionsChange}
      />
    )
    expect(
      await screen.findByText('价格无优势，本次不复制（下次可再选）')
    ).toBeVisible()
    expect(onOptionsChange).not.toHaveBeenCalled()
    expect(screen.getByLabelText('复制 BTCUSDT')).toBeDisabled()
    fireEvent.click(screen.getAllByLabelText('复制 ETHUSDT')[0])
    expect(onOptionsChange).toHaveBeenCalledWith({
      selection: [
        { leader_pos_id: 'p', opened_ms: 1790000000000 },
        { leader_pos_id: 'q', opened_ms: 1790000000000 },
      ],
    })
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
