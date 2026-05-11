import { useEffect } from "react"
import { Card, CardContent } from "@/shared/components/ui/card"
import { Package } from "lucide-react"
import {
  useOrders,
  SubscriptionOrderCard,
  OrdersTable,
  OrderDetailDrawer,
  calculateRefund,
  daysRemaining,
} from "@/features/user-orders"

export default function Orders() {
  const {
    activeTab,
    setActiveTab,
    subs,
    subLoading,
    orders,
    orderLoading,
    orderPage,
    setOrderPage,
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
  } = useOrders()

  useEffect(() => {
    loadSubscriptions()
    loadPaymentOrders(1)
  }, [loadPaymentOrders, loadSubscriptions])

  const orderTotalPages = Math.ceil(orderTotal / 20)

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500" style={{ willChange: 'transform, opacity' }}>
      <div>
        <h2 className="text-2xl font-bold tracking-tight text-gray-900 dark:text-white flex items-center gap-2">
          <Package className="w-6 h-6 text-primary" />
          我的订单
        </h2>
        <p className="text-gray-500 dark:text-dark-300 mt-1">查看您的订阅订单与充值订单记录。</p>
      </div>

      <div className="flex gap-1 p-1 bg-gray-100 dark:bg-dark-800 rounded-xl w-fit">
        <button
          onClick={() => setActiveTab('subscription')}
          className={`px-4 py-2 rounded-lg text-sm font-medium transition-all ${
            activeTab === 'subscription'
              ? 'bg-white dark:bg-dark-700 text-gray-900 dark:text-white shadow-sm'
              : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200'
          }`}
        >
          订阅订单
        </button>
        <button
          onClick={() => setActiveTab('payment')}
          className={`px-4 py-2 rounded-lg text-sm font-medium transition-all ${
            activeTab === 'payment'
              ? 'bg-white dark:bg-dark-700 text-gray-900 dark:text-white shadow-sm'
              : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200'
          }`}
        >
          充值订单
        </button>
      </div>

      {activeTab === 'subscription' ? (
        <div className="space-y-6">
          {subLoading ? (
            <div className="grid gap-6 md:grid-cols-2 lg:grid-cols-3">
              {[1, 2, 3].map((i) => (
                <div key={i} className="h-56 bg-gray-100 dark:bg-dark-800 rounded-xl animate-pulse" />
              ))}
            </div>
          ) : subs.length === 0 ? (
            <Card className="border-dashed border-2 border-gray-200 dark:border-dark-700 bg-gray-50/30 dark:bg-dark-900/30">
              <CardContent className="flex flex-col items-center justify-center py-16 text-center">
                <div className="w-16 h-16 bg-gray-100 dark:bg-dark-800 rounded-full flex items-center justify-center mb-4">
                  <Package className="w-8 h-8 text-gray-400" />
                </div>
                <h4 className="text-lg font-semibold text-gray-700 dark:text-gray-300 mb-2">暂无订单</h4>
                <p className="text-sm text-gray-500 max-w-md">
                  您当前没有任何订阅订单。如需获取订阅，请联系管理员分配或通过专属渠道开通。
                </p>
              </CardContent>
            </Card>
          ) : (
            <div className="grid gap-6 md:grid-cols-2 xl:grid-cols-3">
              {subs.map((s) => {
                const refundAmount = calculateRefund(s)
                const isRefundable = refundAmount > 0 && !refundedSubIds.has(s.id) && s.status === 'active'
                const hasPendingRefund = pendingRefundSubIds.has(s.id)
                const hasCompletedRefund = completedRefundSubIds.has(s.id)
                const remaining = daysRemaining(s.expires_at)

                return (
                  <SubscriptionOrderCard
                    key={s.id}
                    sub={s}
                    isRefundable={isRefundable}
                    hasPendingRefund={hasPendingRefund}
                    hasCompletedRefund={hasCompletedRefund}
                    remainingDays={remaining}
                  />
                )
              })}
            </div>
          )}
        </div>
      ) : (
        <OrdersTable
          orders={orders}
          loading={orderLoading}
          page={orderPage}
          totalPages={orderTotalPages}
          total={orderTotal}
          filterStatus={orderFilterStatus}
          onFilter={handleOrderFilter}
          onPageChange={setOrderPage}
          onRefresh={loadPaymentOrders}
          onSelectOrder={setSelectedOrder}
        />
      )}

      {selectedOrder && (
        <OrderDetailDrawer order={selectedOrder} onClose={() => setSelectedOrder(null)} />
      )}
    </div>
  )
}
