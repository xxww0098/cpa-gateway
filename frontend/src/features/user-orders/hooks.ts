import { useState, useMemo, useCallback } from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { queryKeys } from "@/shared/api/query-keys"
import { errorMessage } from "@/shared/api/errors"
import { toast } from "sonner"
import type { PaymentOrder } from "./types"
import { fetchUserSubscriptions, fetchRefundList, fetchPaymentOrders } from "./api"

// ── useSubscriptionOrders ───────────────────────────────────────────────────

export function useSubscriptionOrders() {
  const subsQuery = useQuery({
    queryKey: queryKeys.orders.subscriptions(),
    queryFn: fetchUserSubscriptions,
  })

  const refundsQuery = useQuery({
    queryKey: queryKeys.orders.refunds(),
    queryFn: fetchRefundList,
  })

  const subs = subsQuery.data ?? []
  const refunds = refundsQuery.data?.items ?? []
  const loading = subsQuery.isLoading || refundsQuery.isLoading

  const pendingRefundSubIds = useMemo(
    () => new Set(refunds.filter((r) => r.status === 'pending').map((r) => r.subscription_id)),
    [refunds]
  )
  const completedRefundSubIds = useMemo(
    () => new Set(refunds.filter((r) => r.status === 'approved').map((r) => r.subscription_id)),
    [refunds]
  )
  const refundedSubIds = useMemo(
    () => new Set([...pendingRefundSubIds, ...completedRefundSubIds]),
    [pendingRefundSubIds, completedRefundSubIds]
  )

  return {
    subs,
    refunds,
    loading,
    refundedSubIds,
    pendingRefundSubIds,
    completedRefundSubIds,
    error: subsQuery.error || refundsQuery.error,
  }
}

// ── usePaymentOrders (with pagination) ──────────────────────────────────────

export function usePaymentOrders(page: number, pageSize: number, status?: string) {
  const query = useQuery({
    queryKey: queryKeys.orders.list({ page, pageSize, status }),
    queryFn: () => fetchPaymentOrders({ page, pageSize, status }),
  })

  if (query.error) {
    toast.error(errorMessage(query.error, '加载订单失败'))
  }

  return {
    orders: query.data?.items ?? [],
    total: query.data?.total ?? 0,
    currentPage: query.data?.page ?? page,
    loading: query.isLoading,
    error: query.error,
    refetch: query.refetch,
  }
}

// ── useOrders (composite hook — maintains backward-compatible interface) ────

export function useOrders() {
  const [activeTab, setActiveTab] = useState<'subscription' | 'payment'>('subscription')
  const [orderPage, setOrderPage] = useState(1)
  const [orderPageSize] = useState(20)
  const [orderFilterStatus, setOrderFilterStatus] = useState('')
  const [selectedOrder, setSelectedOrder] = useState<PaymentOrder | null>(null)

  const qc = useQueryClient()

  // Subscription data
  const {
    subs,
    loading: subLoading,
    refundedSubIds,
    pendingRefundSubIds,
    completedRefundSubIds,
  } = useSubscriptionOrders()

  // Payment orders data (with pagination)
  const ordersQuery = useQuery({
    queryKey: queryKeys.orders.list({ page: orderPage, pageSize: orderPageSize, status: orderFilterStatus || undefined }),
    queryFn: () => fetchPaymentOrders({ page: orderPage, pageSize: orderPageSize, status: orderFilterStatus || undefined }),
  })

  const orders = ordersQuery.data?.items ?? []
  const orderTotal = ordersQuery.data?.total ?? 0
  const orderLoading = ordersQuery.isLoading

  const handleOrderFilter = useCallback((status: string) => {
    setOrderFilterStatus(status)
    setOrderPage(1)
  }, [])

  const loadSubscriptions = useCallback(() => {
    qc.invalidateQueries({ queryKey: queryKeys.orders.subscriptions() })
    qc.invalidateQueries({ queryKey: queryKeys.orders.refunds() })
  }, [qc])

  const loadPaymentOrders = useCallback((_page?: number) => {
    // When called with a page number, update the page state which triggers refetch
    if (_page !== undefined) {
      setOrderPage(_page)
    }
    qc.invalidateQueries({ queryKey: queryKeys.orders.all() })
  }, [qc])

  return {
    activeTab,
    setActiveTab,
    subs,
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
