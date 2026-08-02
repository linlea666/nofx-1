import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { SWRConfig } from 'swr'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LanguageProvider } from '../contexts/LanguageContext'
import { api } from '../lib/api'
import { ComparisonChart } from './ComparisonChart'

vi.mock('../lib/api', () => ({
  api: { getEquityHistoryBatch: vi.fn() },
}))

describe('ComparisonChart request failure', () => {
  beforeEach(() => {
    localStorage.setItem('language', 'en')
    vi.mocked(api.getEquityHistoryBatch).mockReset()
  })

  it('leaves loading state and exposes an explicit retry', async () => {
    vi.mocked(api.getEquityHistoryBatch).mockRejectedValue(
      new Error('Request timeout')
    )
    render(
      <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
        <LanguageProvider>
          <ComparisonChart
            traders={[
              {
                trader_id: 'timeout-trader',
                trader_name: 'Timeout',
              } as any,
            ]}
          />
        </LanguageProvider>
      </SWRConfig>
    )

    expect(await screen.findByText('Failed to load chart')).toBeInTheDocument()
    expect(screen.queryByText('Loading chart data...')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() =>
      expect(api.getEquityHistoryBatch).toHaveBeenCalledTimes(2)
    )
  })
})
