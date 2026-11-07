export const AUTH_INVALIDATED = 'droidharbor:auth-invalidated'
let csrfToken = ''
export function setCSRFToken(value: string) { csrfToken = value }
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string
  constructor(status: number, code: string, message: string, requestId = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
  }
}
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return `${error.message}${error.requestId ? `（请求 ${error.requestId}）` : ''}`
  if (error instanceof Error) return error.message
  return '请求失败'
}
export async function apiResponse(path: string, options: RequestInit = {}): Promise<Response> {
  const method = (options.method ?? 'GET').toUpperCase()
  const headers = new Headers(options.headers)
  if (method !== 'GET' && method !== 'HEAD' && csrfToken) headers.set('X-CSRF-Token', csrfToken)
  let response: Response
  try {
    response = await fetch('/api/v1' + path, { ...options, method, headers, credentials: 'same-origin', cache: 'no-store' })
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new ApiError(0, method === 'GET' || method === 'HEAD' ? 'NETWORK_UNAVAILABLE' : 'RESULT_UNKNOWN', method === 'GET' || method === 'HEAD' ? '无法连接平台，请检查网络后重新读取状态。' : '请求结果未知，请先核对当前状态，不要直接重复操作。')
  }
  if (response.ok) return response
  if (response.status === 401 && path !== '/admin/sessions') window.dispatchEvent(new Event(AUTH_INVALIDATED))
  const payload = await response.json().catch(() => null) as { code?: string; message?: string; requestId?: string } | null
  throw new ApiError(response.status, payload?.code ?? 'HTTP_ERROR', payload?.message ?? `请求失败（HTTP ${response.status}）`, payload?.requestId ?? response.headers.get('X-Request-ID') ?? '')
}
export async function api<T>(path: string, options: { method?: string; body?: unknown; signal?: AbortSignal; keepalive?: boolean } = {}): Promise<T> {
  const response = await apiResponse(path, {
    method: options.method,
    signal: options.signal,
    keepalive: options.keepalive,
    headers: options.body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  })
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}
