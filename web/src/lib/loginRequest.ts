export const LOGIN_REQUEST_TIMEOUT_MS = 15000

export interface LoginRequestResult {
  success: boolean
  message?: string
  userID?: string
  requiresOTP?: boolean
}

type FetchLike = typeof fetch

export async function requestLogin(
  email: string,
  password: string,
  fetchImpl: FetchLike = fetch,
  timeoutMs = LOGIN_REQUEST_TIMEOUT_MS
): Promise<LoginRequestResult> {
  const controller = new AbortController()
  let timedOut = false
  const timeout = window.setTimeout(() => {
    timedOut = true
    controller.abort()
  }, timeoutMs)

  try {
    const response = await fetchImpl('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password }),
      signal: controller.signal,
    })

    const raw = await response.text()
    let data: Record<string, any> = {}
    if (raw) {
      try {
        data = JSON.parse(raw)
      } catch {
        // Reverse proxies may return HTML for 5xx responses. Status handling
        // below still produces a stable user-facing message.
      }
    }

    if (response.status === 401) {
      return {
        success: false,
        message: data.error || '邮箱或密码错误',
      }
    }
    if (
      response.status === 503 ||
      response.status === 504 ||
      data.code === 'DATABASE_UNAVAILABLE'
    ) {
      return { success: false, message: '服务暂不可用，请稍后重试' }
    }
    if (!response.ok) {
      return {
        success: false,
        message: data.error || data.message || '登录失败，请重试',
      }
    }
    if (data.requires_otp) {
      return {
        success: true,
        userID: data.user_id,
        requiresOTP: true,
        message: data.message,
      }
    }
    return { success: false, message: '服务响应异常，请稍后重试' }
  } catch (error) {
    if (
      timedOut ||
      (error instanceof DOMException && error.name === 'AbortError')
    ) {
      return { success: false, message: '登录请求超时，请稍后重试' }
    }
    return { success: false, message: '网络连接失败，请检查网络后重试' }
  } finally {
    window.clearTimeout(timeout)
  }
}
