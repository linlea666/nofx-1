import { afterEach, expect, it, vi } from 'vitest'
import { api } from './api'
import { httpClient } from './httpClient'

afterEach(() => vi.restoreAllMocks())

it('uses the authorized trader query and preserves an explicit empty running list', async () => {
  const request = vi
    .spyOn(httpClient, 'get')
    .mockResolvedValue({ success: true, data: { traders: [] } })
  await expect(api.getCopyRuntimeHealth('trader/a b')).resolves.toEqual({
    traders: [],
  })
  expect(request).toHaveBeenCalledWith(
    '/api/copytrade/runtime-health?trader_id=trader%2Fa+b'
  )
})

it('rejects incomplete health responses instead of returning a false healthy empty list', async () => {
  const request = vi
    .spyOn(httpClient, 'get')
    .mockResolvedValue({ success: true, data: {} })
  await expect(api.getCopyRuntimeHealth()).rejects.toThrow('响应不完整')
  request.mockResolvedValue({
    success: true,
    data: { traders: [{ trader_id: 't', running: true }] },
  })
  await expect(api.getCopyRuntimeHealth()).rejects.toThrow('响应不完整')
})

it('propagates service errors to the runtime panel', async () => {
  vi.spyOn(httpClient, 'get').mockResolvedValue({
    success: false,
    message: 'source health unavailable',
  })
  await expect(api.getCopyRuntimeHealth()).rejects.toThrow(
    'source health unavailable'
  )
})
