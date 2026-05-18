import { Package } from "lucide-react"
import { QueryStateWrapper } from "@/shared/components/QueryStateWrapper"
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
    loadPaymentOrders,
  } = useOrders()

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
          <QueryStateWrapper
            isLoading={subLoading}
            isEmpty={!subLoading && subs.length === 0}
            emptyMessage="您当前没有任何订阅订单。如需获取订阅，请联系管理员分配或通过专属渠道开通。"
          >
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
          </QueryStateWrapper>
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
