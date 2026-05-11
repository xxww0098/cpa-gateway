import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fetchApi, isAbortError } from '@/shared/api/client'
import { useAuthStore } from '@/features/auth/auth_store'

// Mock global fetch
globalThis.fetch = vi.fn()

// Mock Zustand store
vi.mock('@/features/auth/auth_store', () => ({
  useAuthStore: {
    getState: vi.fn()
  }
}))

describe('fetchApi User API Client', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    
    // Default mock store
    vi.mocked(useAuthStore.getState).mockReturnValue({
      token: 'fake-jwt-token',
      logout: vi.fn(),
    } as unknown as ReturnType<typeof useAuthStore.getState>)
  })

  it('should attach Bearer token if present in auth store', async () => {
    vi.mocked(globalThis.fetch).mockResolvedValueOnce({
      ok: true,
      text: async () => JSON.stringify({ code: 0, data: 'success', message: 'ok' })
    } as unknown as Response)

    const res = await fetchApi('/test-endpoint')
    
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/panel/test-endpoint', expect.objectContaining({
      headers: expect.any(Headers)
    }))
    
    const passedOptions = vi.mocked(globalThis.fetch).mock.calls[0]?.[1] || {}
    const headers = passedOptions.headers as Headers
    expect(headers.get('Authorization')).toBe('Bearer fake-jwt-token')
    expect(res).toEqual({ code: 0, data: 'success', message: 'ok' })
  })

  it('should handle standard wrapped response and return raw data if wrapped', async () => {
    vi.mocked(globalThis.fetch).mockResolvedValueOnce({
      ok: true,
      text: async () => JSON.stringify({ code: 0, data: { bal: 10 }, message: 'ok' })
    } as unknown as Response)

    const res = await fetchApi('/profile')
    // As per api.ts: `if (data && typeof data === 'object' && 'code' in data) { return data }`
    // So it should return the exact wrapped struct.
    expect(res).toEqual({ code: 0, data: { bal: 10 }, message: 'ok' })
  })

  it('should intercept 401 Unauthorized and implicitly call logout()', async () => {
    const logoutMock = vi.fn()
    vi.mocked(useAuthStore.getState).mockReturnValue({
      token: 'expired-token',
      logout: logoutMock,
    } as unknown as ReturnType<typeof useAuthStore.getState>)

    vi.mocked(globalThis.fetch).mockResolvedValueOnce({
      ok: false,
      status: 401,
      statusText: 'Unauthorized',
      text: async () => JSON.stringify({ message: "token_expired" })
    } as unknown as Response)

    // fetchApi throws Error when not OK
    await expect(fetchApi('/secret-page')).rejects.toThrow('token_expired')
    expect(logoutMock).toHaveBeenCalled()
  })

  it('should bubble up custom Error messages from non-OK statuses', async () => {
    vi.mocked(globalThis.fetch).mockResolvedValueOnce({
      ok: false,
      status: 400,
      text: async () => JSON.stringify({ error: { message: "Invalid parameters" } })
    } as unknown as Response)

    await expect(fetchApi('/submit')).rejects.toThrow("Invalid parameters")
  })
})

describe('isAbortError', () => {
  it('should return true for DOMException with name AbortError', () => {
    expect(isAbortError(new DOMException('aborted', 'AbortError'))).toBe(true)
  })

  it('should return true for Error with name AbortError', () => {
    const err = new Error('aborted')
    err.name = 'AbortError'
    expect(isAbortError(err)).toBe(true)
  })

  it('should return false for regular errors', () => {
    expect(isAbortError(new Error('network error'))).toBe(false)
  })

  it('should return false for non-error values', () => {
    expect(isAbortError(null)).toBe(false)
    expect(isAbortError('string')).toBe(false)
    expect(isAbortError(undefined)).toBe(false)
  })
})

describe('fetchApi signal support', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(useAuthStore.getState).mockReturnValue({
      token: 'fake-jwt-token',
      logout: vi.fn(),
    } as unknown as ReturnType<typeof useAuthStore.getState>)
  })

  it('should pass signal to underlying fetch', async () => {
    const controller = new AbortController()
    vi.mocked(globalThis.fetch).mockResolvedValueOnce({
      ok: true,
      text: async () => JSON.stringify({ code: 0, data: 'ok', message: 'ok' })
    } as unknown as Response)

    await fetchApi('/test', { signal: controller.signal })

    const passedOptions = vi.mocked(globalThis.fetch).mock.calls[0]?.[1] || {}
    expect(passedOptions.signal).toBe(controller.signal)
  })

  it('should propagate AbortError from fetch', async () => {
    const abortError = new DOMException('The operation was aborted.', 'AbortError')
    vi.mocked(globalThis.fetch).mockRejectedValueOnce(abortError)

    await expect(fetchApi('/test')).rejects.toThrow(abortError)
  })
})
