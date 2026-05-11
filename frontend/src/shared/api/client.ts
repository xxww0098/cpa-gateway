import { useAuthStore } from "@/features/auth/auth_store"

/** Returns true if the error is from an aborted fetch request. */
export function isAbortError(err: unknown): boolean {
  return typeof err === 'object' && err !== null && 'name' in err && err.name === 'AbortError'
}

export async function fetchApi(endpoint: string, options: RequestInit = {}) {
  const token = useAuthStore.getState().token
  
  const headers = new Headers(options.headers || {})
  headers.set('Content-Type', 'application/json')
  if (token) {
    headers.set('Authorization', `Bearer ${token}`)
  }

  const response = await fetch(`/api/panel${endpoint}`, {
    ...options,
    headers,
  })

  const rawText = await response.text()
  let data
  try {
    data = JSON.parse(rawText)
  } catch {
    data = null
  }

  if (!response.ok) {
    if (response.status === 401) {
      useAuthStore.getState().logout()
    }
    const errField = data?.error
    const errStr = typeof errField === 'string' ? errField : errField?.message
    const message = data?.msg || data?.message || errStr || response.statusText || '请求异常'
    throw new Error(message)
  }

  // Assuming our standard wrapper { code: 0, message: "成功", data: ... }
  if (data && typeof data === 'object' && 'code' in data) {
    return data
  }
  
  return data
}

export function errorMessage(error: unknown, fallback = '请求异常') {
  return error instanceof Error ? error.message : fallback
}

export async function refreshCurrentUser() {
  const token = useAuthStore.getState().token
  if (!token) return null

  const res = await fetchApi('/user/profile')
  if (res?.data) {
    const user = res.data.user ?? res.data
    useAuthStore.getState().updateUser(user)
    return user
  }
  return null
}

/** multipart/form-data，勿设置 Content-Type（由浏览器带 boundary） */
export async function fetchApiFormData(endpoint: string, formData: FormData) {
  const token = useAuthStore.getState().token
  const headers = new Headers()
  if (token) {
    headers.set('Authorization', `Bearer ${token}`)
  }
  const response = await fetch(`/api/panel${endpoint}`, {
    method: 'POST',
    body: formData,
    headers,
  })
  const rawText = await response.text()
  let data: unknown
  try {
    data = JSON.parse(rawText)
  } catch {
    data = null
  }
  if (!response.ok) {
    if (response.status === 401) {
      useAuthStore.getState().logout()
    }
    const d = data as { msg?: string; message?: string; error?: string | { message?: string } } | null
    const errField = d?.error
    const errStr = typeof errField === 'string' ? errField : errField?.message
    const message = d?.msg || d?.message || errStr || response.statusText || '请求异常'
    throw new Error(message)
  }
  if (data && typeof data === 'object' && 'code' in (data as object)) {
    return data as { code: number; message?: string; data?: Record<string, unknown> }
  }
  return data
}

/** 已登录下载二进制（如同源相对路径 /api/panel/...） */
export async function fetchApiBlob(pathOrUrl: string): Promise<Blob> {
  const token = useAuthStore.getState().token
  const headers = new Headers()
  if (token) {
    headers.set('Authorization', `Bearer ${token}`)
  }
  const url =
    pathOrUrl.startsWith('http://') || pathOrUrl.startsWith('https://')
      ? pathOrUrl
      : pathOrUrl.startsWith('/api/')
        ? pathOrUrl
        : `/api/panel${pathOrUrl.startsWith('/') ? pathOrUrl : `/${pathOrUrl}`}`
  const response = await fetch(url, { headers })
  if (!response.ok) {
    if (response.status === 401) {
      useAuthStore.getState().logout()
    }
    throw new Error(response.statusText || '加载失败')
  }
  return response.blob()
}
