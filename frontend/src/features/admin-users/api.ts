import { fetchApi } from '@/shared/api/client'
import type {
  ManagedApiKey,
  BalanceHistoryEntry,
  PageData,
  CreateUserPayload,
  UpdateUserPayload,
  DepositPayload,
} from './types'

export interface LoadUsersParams {
  page: number
  pageSize?: number
  search?: string
  role?: string
  status?: string
}

export async function loadUsers(params: LoadUsersParams): Promise<PageData> {
  const { page, pageSize = 15, search, role, status } = params
  const qs = new URLSearchParams()
  qs.set('page', String(page))
  qs.set('page_size', String(pageSize))
  if (search) qs.set('q', search)
  if (role) qs.set('role', role)
  if (status) qs.set('status', status)

  const res = await fetchApi(`/admin/users?${qs.toString()}`)
  return res.data as PageData
}

export async function createUser(payload: CreateUserPayload): Promise<void> {
  await fetchApi('/admin/users', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function updateUser(id: number, payload: UpdateUserPayload): Promise<void> {
  await fetchApi(`/admin/users/${id}`, {
    method: 'PUT',
    body: JSON.stringify(payload),
  })
}

export async function deleteUser(id: number): Promise<void> {
  await fetchApi(`/admin/users/${id}`, {
    method: 'DELETE',
  })
}

export async function depositUser(id: number, payload: DepositPayload): Promise<void> {
  await fetchApi(`/admin/users/${id}/deposit`, {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function getUserApiKeys(id: number): Promise<ManagedApiKey[]> {
  const res = await fetchApi(`/admin/users/${id}/api-keys`)
  return (res.data as ManagedApiKey[]) || []
}

export async function getUserBalanceHistory(id: number): Promise<BalanceHistoryEntry[]> {
  const res = await fetchApi(`/admin/users/${id}/balance-history`)
  return res.data?.entries || []
}
