import { useState, useCallback } from "react"
import { errorMessage, fetchApi } from "@/shared/api/client"
import { toast } from "sonner"
import type { Subscription, RefundRecord, PaymentOrder } from "./types"

export function useOrders() {
  const [activeTab, setActiveTab] = useState<'subscription' | 'payment'>('subscription')

  const [subs, setSubs] = useState<Subscription[]>([])
  const [refunds, setRefunds] = useState<RefundRecord[]>([])
  const [subLoading, setSubLoading] = useState(true)

  const [orders, setOrders] = useState<PaymentOrder[]>([])
  const [orderLoading, setOrderLoading] = useState(true)
  const [orderPage, setOrderPage] = useState(1)
  const [orderPageSize] = useState(20)
  const [orderTotal, setOrderTotal] = useState(0)
  const [orderFilterStatus, setOrderFilterStatus] = useState('')
  const [selectedOrder, setSelectedOrder] = useState<PaymentOrder | null>(null)

  const loadSubscriptions = useCallback(async () => {
    setSubLoading(true)
    try {
      const [subsRes, refundsRes] = await Promise.all([
        fetchApi('/user/subscriptions'),
        fetchApi('/refund/list').catch(() => ({ data: { items: [] } })),
      ])
      setSubs(subsRes?.data || [])
      setRefunds(refundsRes?.data?.items || [])
    } catch (err: unknown) {
      toast.error(errorMessage(err, '加载失败'))
    } finally {
      setSubLoading(false)
    }
  }, [])

  const loadPaymentOrders = useCallback(async (p = 1) => {
    setOrderLoading(true)
    try {
      const params = new URLSearchParams()
      params.set('page', String(p))
      params.set('page_size', String(orderPageSize))
      if (orderFilterStatus) params.set('status', orderFilterStatus)
      const res = await fetchApi(`/user/orders?${params}`)
      if (res?.data) {
        setOrders(res.data.items || [])
        setOrderTotal(res.data.total || 0)
        setOrderPage(res.data.page || p)
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '加载订单失败')
    } finally {
      setOrderLoading(false)
    }
  }, [orderPageSize, orderFilterStatus])

  const handleOrderFilter = (status: string) => {
    setOrderFilterStatus(status)
    setOrderPage(1)
    setTimeout(() => loadPaymentOrders(1), 0)
  }

  const pendingRefundSubIds = new Set(
    refunds.filter((r) => r.status === 'pending').map((r) => r.subscription_id)
  )
  const completedRefundSubIds = new Set(
    refunds.filter((r) => r.status === 'approved').map((r) => r.subscription_id)
  )
  const refundedSubIds = new Set([...pendingRefundSubIds, ...completedRefundSubIds])

  return {
    activeTab,
    setActiveTab,
    subs,
    refunds,
    subLoading,
    orders,
    orderLoading,
    orderPage,
    setOrderPage,
    orderPageSize,
    orderTotal,
    orderFilterStatus,
    handleOrderFilter,
    selectedOrder,
    setSelectedOrder,
    refundedSubIds,
    pendingRefundSubIds,
    completedRefundSubIds,
    loadSubscriptions,
    loadPaymentOrders,
  }
}
