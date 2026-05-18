// Types for payment feature module

// ── Wechat Pay ──────────────────────────────────────────────────────────────

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

// ── Alipay ──────────────────────────────────────────────────────────────────

export interface AlipayCreateRequest {
  amount: number
}

export interface AlipayCreateResponse {
  order_id: string
  pay_url: string
  qr_code: string
  amount_usd: number
  amount_local: number
  currency: string
}

export interface AlipayStatusResponse {
  status: "pending" | "paid" | "failed"
  order_id: string
  amount: number
  paid_at?: string
}

// ── Stripe ──────────────────────────────────────────────────────────────────

export interface StripeConfig {
  publishable_key: string
  mode: string
  enabled: boolean
}

export interface StripeCreateRequest {
  amount: number
  currency: string
}

export interface StripeCreateResponse {
  client_secret: string
  order_id: string
  payment_intent_id: string
  amount_usd: number
}

// ── Shared ──────────────────────────────────────────────────────────────────

export type PaymentStatus = "pending" | "paid" | "failed"
