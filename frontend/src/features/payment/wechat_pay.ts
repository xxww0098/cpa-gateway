import { fetchApi } from "@/shared/api/client"

export interface WechatCreateRequest {
  amount: number
}

export interface WechatCreateResponse {
  order_id: string
  code_url: string
  amount_usd: number
  amount_local: number
  currency: string
}

export interface WechatStatusResponse {
  status: "pending" | "paid" | "failed"
  order_id: string
  amount: number
  paid_at?: string
}

export async function createWechatOrder(amount: number): Promise<WechatCreateResponse> {
  const res = await fetchApi("/payment/wechat/create", {
    method: "POST",
    body: JSON.stringify({ amount }),
  })
  if (res.code !== 0) {
    throw new Error(res.message || "创建微信支付订单失败")
  }
  return res.data as WechatCreateResponse
}

export async function getWechatOrderStatus(orderId: string): Promise<WechatStatusResponse> {
  const res = await fetchApi(`/payment/wechat/status?order_id=${encodeURIComponent(orderId)}`, {
    method: "GET",
  })
  if (res.code !== 0) {
    throw new Error(res.message || "查询订单状态失败")
  }
  return res.data as WechatStatusResponse
}
