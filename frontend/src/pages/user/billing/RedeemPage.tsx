import { useState, useCallback } from "react"
import { useAuthStore } from "@/features/auth/auth_store"
import { fetchApi, refreshCurrentUser } from "@/shared/api/client"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/shared/components/ui/card"
import { Button } from "@/shared/components/ui/button"
import { Input } from "@/shared/components/ui/input"
import { toast } from "sonner"
import { Gift, Wallet } from "lucide-react"
import StripePayment from "@/features/payment/components/StripePayment"
import WechatPaySection from "@/features/payment/components/WechatPaySection"
import AlipayPayment from "@/features/payment/components/AlipayPayment"

export default function Redeem() {
  const user = useAuthStore(s => s.user)
  const [code, setCode] = useState("")
  const [loading, setLoading] = useState(false)

  const refreshBalance = useCallback(async () => {
    try {
      await refreshCurrentUser()
    } catch (err: unknown) {
      console.error("Refresh balance failed:", err)
    }
  }, [])

  const handleRedeem = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!code.trim()) return

    setLoading(true)
    try {
      const res = await fetchApi("/user/redeem", {
        method: "POST",
        body: JSON.stringify({ code: code.trim() }),
      })
      toast.success(`充值成功！您的账户增加了 $${res.data.amount.toFixed(4)}`)
      setCode("")
      await refreshBalance()
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "兑换失败，请检查兑换码是否有效")
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="space-y-6 max-w-2xl mx-auto animate-in fade-in slide-in-from-bottom-4 duration-500" style={{ willChange: 'transform, opacity' }}>
      <div className="text-center md:text-left">
        <h2 className="text-3xl font-bold tracking-tight text-foreground">充值与兑换</h2>
        <p className="text-muted-foreground mt-1">使用兑换码、支付宝或微信支付为主账户增加可用余额。</p>
      </div>

      <div className="grid gap-6 md:grid-cols-2">
        <Card className="shadow-sm border-border order-2 md:order-1 flex flex-col justify-center items-center p-6 bg-gradient-to-br from-primary/5 to-secondary/30">
          <Wallet className="h-12 w-12 text-primary mb-4" />
          <div className="text-sm text-muted-foreground mb-1">当前账户余额</div>
          <div className="text-4xl font-bold text-foreground">
            ${user?.balance?.toFixed(4) || "0.00"}
          </div>
        </Card>

        <Card className="shadow-sm border-border order-1 md:order-2">
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Gift className="h-5 w-5 text-primary" />
              使用兑换码
            </CardTitle>
            <CardDescription>
              请输入 16 位或 32 位的充值卡密。
            </CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={handleRedeem} className="space-y-4">
              <div className="space-y-2">
                <Input
                  placeholder="例如：CPA-a1b2c3d4..."
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  className="font-mono text-sm"
                  required
                />
              </div>
              <Button type="submit" className="w-full" disabled={loading || !code.trim()}>
                {loading ? "处理中..." : "立即充值"}
              </Button>
            </form>
          </CardContent>
        </Card>
      </div>

      <StripePayment onSuccess={refreshBalance} />
      <AlipayPayment onSuccess={refreshBalance} />
      <WechatPaySection onSuccess={refreshBalance} />
    </div>
  )
}
