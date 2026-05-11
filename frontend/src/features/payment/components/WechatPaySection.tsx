import { useState, useEffect, useCallback, useRef } from "react"
import { QRCodeSVG } from "qrcode.react"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/shared/components/ui/card"
import { Button } from "@/shared/components/ui/button"
import { Input } from "@/shared/components/ui/input"
import { toast } from "sonner"
import { MessageSquare, Loader2, CheckCircle2, XCircle, RefreshCw } from "lucide-react"
import { createWechatOrder, getWechatOrderStatus, type WechatCreateResponse } from "@/features/payment/wechat_pay"

interface WechatPaySectionProps {
  onSuccess?: () => void
}

export default function WechatPaySection({ onSuccess }: WechatPaySectionProps) {
  const [amount, setAmount] = useState("")
  const [loading, setLoading] = useState(false)
  const [order, setOrder] = useState<WechatCreateResponse | null>(null)
  const [status, setStatus] = useState<"pending" | "paid" | "failed" | null>(null)
  const pollTimerRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const stopPolling = useCallback(() => {
    if (pollTimerRef.current) {
      clearInterval(pollTimerRef.current)
      pollTimerRef.current = null
    }
  }, [])

  const startPolling = useCallback(
    (orderId: string) => {
      stopPolling()
      setStatus("pending")

      const check = async () => {
        try {
          const res = await getWechatOrderStatus(orderId)
          setStatus(res.status)

          if (res.status === "paid") {
            stopPolling()
            toast.success(`微信支付成功！已充值 $${res.amount.toFixed(2)}`)
            onSuccess?.()
          } else if (res.status === "failed") {
            stopPolling()
            toast.error("支付失败，请重试")
          }
        } catch (err: unknown) {
          // Keep polling on network errors; don't show toast every time
          console.error("Poll status error:", err)
        }
      }

      check()
      pollTimerRef.current = setInterval(check, 3000)
    },
    [stopPolling, onSuccess]
  )

  useEffect(() => {
    return () => {
      stopPolling()
    }
  }, [stopPolling])

  const handleCreateOrder = async () => {
    const val = parseFloat(amount)
    if (!val || val <= 0 || isNaN(val)) {
      toast.error("请输入有效的充值金额")
      return
    }

    setLoading(true)
    try {
      const res = await createWechatOrder(val)
      setOrder(res)
      startPolling(res.order_id)
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "创建订单失败")
    } finally {
      setLoading(false)
    }
  }

  const handleReset = () => {
    stopPolling()
    setOrder(null)
    setStatus(null)
    setAmount("")
  }

  return (
    <Card className="shadow-sm border-border">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <MessageSquare className="h-5 w-5 text-green-600" />
          微信支付
        </CardTitle>
        <CardDescription>
          使用微信扫码支付，即时到账。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {!order && (
          <div className="space-y-4">
            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <label className="text-sm font-medium text-foreground">充值金额 (USD)</label>
                <span className="text-xs text-muted-foreground">最低 $1</span>
              </div>
              <div className="flex gap-2">
                <Input
                  type="number"
                  min={1}
                  step={1}
                  placeholder="例如：50"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                  className="flex-1"
                  disabled={loading}
                />
                <Button
                  onClick={handleCreateOrder}
                  disabled={loading || !amount}
                  className="shrink-0"
                >
                  {loading ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : (
                    "生成二维码"
                  )}
                </Button>
              </div>
            </div>

            <div className="flex gap-2">
              {[10, 50, 100, 500].map((preset) => (
                <Button
                  key={preset}
                  variant="outline"
                  size="sm"
                  onClick={() => setAmount(String(preset))}
                  disabled={loading}
                  className="flex-1"
                >
                  ${preset}
                </Button>
              ))}
            </div>
          </div>
        )}

        {order && (
          <div className="flex flex-col items-center space-y-4 animate-in fade-in zoom-in-95 duration-300">
            {status === "paid" ? (
              <div className="flex flex-col items-center space-y-3 py-6">
                <CheckCircle2 className="h-16 w-16 text-green-500" />
                <div className="text-xl font-bold text-foreground">支付成功</div>
                <div className="text-sm text-muted-foreground">
                  已充值 ${order.amount_usd.toFixed(2)} USD
                </div>
                <Button variant="outline" onClick={handleReset} className="mt-2">
                  <RefreshCw className="h-4 w-4 mr-1" />
                  继续充值
                </Button>
              </div>
            ) : status === "failed" ? (
              <div className="flex flex-col items-center space-y-3 py-6">
                <XCircle className="h-16 w-16 text-destructive" />
                <div className="text-xl font-bold text-foreground">支付失败</div>
                <div className="text-sm text-muted-foreground">
                  订单未成功，请检查支付状态或重试
                </div>
                <Button variant="outline" onClick={handleReset} className="mt-2">
                  <RefreshCw className="h-4 w-4 mr-1" />
                  重新支付
                </Button>
              </div>
            ) : (
              <>
                <div className="rounded-xl border border-border bg-white p-4 shadow-sm">
                  <QRCodeSVG
                    value={order.code_url}
                    size={200}
                    level="M"
                    includeMargin={false}
                  />
                </div>
                <div className="text-center space-y-1">
                  <div className="text-sm font-medium text-foreground">
                    请使用微信扫一扫
                  </div>
                  <div className="text-xs text-muted-foreground">
                    订单金额：${order.amount_usd.toFixed(2)} USD
                    {order.currency !== "USD" && (
                      <span>（约 {order.amount_local.toFixed(2)} {order.currency}）</span>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-2 text-xs text-muted-foreground">
                  <Loader2 className="h-3 w-3 animate-spin" />
                  等待支付…
                </div>
              </>
            )}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
