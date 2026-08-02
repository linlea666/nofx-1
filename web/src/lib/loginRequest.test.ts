import { describe, expect, it, vi } from 'vitest'
import { requestLogin } from './loginRequest'

const response = (
  status: number,
  body: string,
  contentType = 'application/json'
) =>
  Promise.resolve(
    new Response(body, { status, headers: { 'Content-Type': contentType } })
  )

describe('requestLogin', () => {
  it('returns credential error for 401 JSON', async () => {
    const fetcher = vi.fn(() =>
      response(401, JSON.stringify({ error: 'Email or password incorrect' }))
    ) as unknown as typeof fetch
    const result = await requestLogin('user@example.com', 'bad', fetcher)
    expect(result).toEqual({
      success: false,
      message: 'Email or password incorrect',
    })
  })

  it.each([503, 504])(
    'returns service unavailable for %s including an HTML gateway page',
    async (status) => {
      const fetcher = vi.fn(() =>
        response(status, '<html>gateway error</html>', 'text/html')
      ) as unknown as typeof fetch
      const result = await requestLogin('user@example.com', 'secret', fetcher)
      expect(result).toEqual({
        success: false,
        message: '服务暂不可用，请稍后重试',
      })
    }
  )

  it('recognizes the stable database error code', async () => {
    const fetcher = vi.fn(() =>
      response(
        503,
        JSON.stringify({ code: 'DATABASE_UNAVAILABLE', error: 'unavailable' })
      )
    ) as unknown as typeof fetch
    const result = await requestLogin('user@example.com', 'secret', fetcher)
    expect(result.message).toBe('服务暂不可用，请稍后重试')
  })

  it('aborts and reports a request timeout', async () => {
    const fetcher = vi.fn(
      (_url: RequestInfo | URL, init?: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () =>
            reject(new DOMException('aborted', 'AbortError'))
          )
        })
    ) as unknown as typeof fetch
    const result = await requestLogin('user@example.com', 'secret', fetcher, 5)
    expect(result).toEqual({
      success: false,
      message: '登录请求超时，请稍后重试',
    })
  })
})
