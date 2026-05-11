import { describe, it, expect, vi, beforeEach } from 'vitest'
import { fetchMgmtApi, fetchMgmtApiFormData } from './api'
import * as api from '@/shared/api/client'

// Mock auth store to avoid localStorage errors at import time
vi.mock('@/features/auth/auth_store', () => ({
  useAuthStore: {
    getState: vi.fn()
  }
}))

// We want to spy on fetchApi, so we mock the api module partially
vi.mock('@/shared/api/client', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/shared/api/client')>()
  return {
    ...actual,
    fetchApi: vi.fn(),
    fetchApiFormData: vi.fn(),
  }
})

describe('fetchMgmtApi CLI Proxy API SDK Client', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('routes JSON SDK management requests through the CPA admin proxy', async () => {
    vi.mocked(api.fetchApi).mockResolvedValueOnce({
      success: true,
      message: 'config updated',
    })

    const res = await fetchMgmtApi('/config')

    expect(api.fetchApi).toHaveBeenCalledWith('/admin/sdk-management/config', {})
    expect(res).toEqual({ success: true, message: 'config updated' })
  })

  it('normalizes SDK management paths without a leading slash', async () => {
    vi.mocked(api.fetchApi).mockResolvedValueOnce({ users: [] })

    await fetchMgmtApi('users')

    expect(api.fetchApi).toHaveBeenCalledWith('/admin/sdk-management/users', {})
  })

  it('routes multipart SDK management requests through the CPA admin proxy', async () => {
    const form = new FormData()
    form.append('file', new Blob(['{}'], { type: 'application/json' }), 'auth.json')
    vi.mocked(api.fetchApiFormData).mockResolvedValueOnce({ ok: true })

    const res = await fetchMgmtApiFormData('/auth-files', form)

    expect(api.fetchApiFormData).toHaveBeenCalledWith('/admin/sdk-management/auth-files', form)
    expect(res).toEqual({ ok: true })
  })
})
