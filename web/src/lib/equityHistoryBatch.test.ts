import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './api'
import { httpClient } from './httpClient'

describe('getEquityHistoryBatch', () => {
  afterEach(() => vi.restoreAllMocks())

  it('turns an all-database-error payload into a chart-visible failure', async () => {
    vi.spyOn(httpClient, 'request').mockResolvedValue({
      success: true,
      data: {
        histories: {},
        errors: {
          one: 'temporarily unavailable',
          two: 'temporarily unavailable',
        },
      },
    })

    await expect(api.getEquityHistoryBatch(['one', 'two'])).rejects.toThrow(
      '历史数据服务暂时不可用'
    )
  })

  it('preserves partial history when only one trader fails', async () => {
    const payload = {
      histories: { one: [{ total_equity: 100 }] },
      errors: { two: 'temporarily unavailable' },
    }
    vi.spyOn(httpClient, 'request').mockResolvedValue({
      success: true,
      data: payload,
    })

    await expect(api.getEquityHistoryBatch(['one', 'two'])).resolves.toBe(
      payload
    )
  })
})
