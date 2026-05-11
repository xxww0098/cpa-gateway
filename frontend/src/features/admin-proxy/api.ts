import { fetchApi, fetchApiFormData } from '@/shared/api/client'

function sdkManagementEndpoint(endpoint: string) {
  const suffix = endpoint.startsWith('/') ? endpoint : `/${endpoint}`
  return `/admin/sdk-management${suffix}`
}

// 代理管理接口的通用封装
export async function fetchMgmtApi(endpoint: string, options: RequestInit = {}) {
  return fetchApi(sdkManagementEndpoint(endpoint), options)
}

/** multipart/form-data（如批量上传 auth-files），勿设置 Content-Type，由浏览器带 boundary */
export async function fetchMgmtApiFormData(endpoint: string, formData: FormData) {
  return fetchApiFormData(sdkManagementEndpoint(endpoint), formData)
}
