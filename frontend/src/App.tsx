import { lazy, Suspense, type ReactNode } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { Toaster } from 'sonner'
import { ConfirmModalProvider } from '@/shared/confirm-modal'
import { ErrorBoundary } from '@/shared/components/ErrorBoundary'
import { AuthLayout } from './pages/public/AuthLayout'
import UserLayout from './pages/user/UserLayout'

const Home = lazy(() => import('./pages/public/HomePage'))
const Login = lazy(() => import('./pages/public/LoginPage'))
const Register = lazy(() => import('./pages/public/RegisterPage'))
const Dashboard = lazy(() => import('./pages/user/dashboard/DashboardPage'))
const Keys = lazy(() => import('./pages/user/api-keys/ApiKeysPage'))
const Models = lazy(() => import('./pages/user/models/ModelsPage'))
const Usage = lazy(() => import('./pages/user/usage/UsagePage'))
const BalanceHistory = lazy(() => import('./pages/user/billing/BalanceHistoryPage'))
const Subscriptions = lazy(() => import('./pages/user/subscriptions/SubscriptionsPage'))
const Orders = lazy(() => import('./pages/user/orders/OrdersPage'))
const Refunds = lazy(() => import('./pages/user/refunds/RefundsPage'))
const RefundApply = lazy(() => import('./pages/user/refunds/RefundApplyPage'))
const Tickets = lazy(() => import('./pages/user/tickets/TicketsPage'))
const Redeem = lazy(() => import('./pages/user/billing/RedeemPage'))

const AdminUsers = lazy(() => import('./pages/admin/users/AdminUsersPage'))
const AdminChannels = lazy(() => import('./pages/admin/proxy/AdminProxyChannelsPage'))
const AdminBilling = lazy(() => import('./pages/admin/billing/AdminBillingPage'))
const AdminUsageLogs = lazy(() => import('./pages/admin/usage-logs/AdminUsageLogsPage'))
const AdminSettings = lazy(() => import('./pages/admin/settings/AdminSettingsPage'))
const AdminPaymentConfig = lazy(() => import('./pages/admin/payment-config/AdminPaymentConfigPage'))
const AdminOrders = lazy(() => import('./pages/admin/orders/AdminOrdersPage'))
const AdminRefunds = lazy(() => import('./pages/admin/refunds/AdminRefundsPage'))
const AdminSubscriptions = lazy(() => import('./pages/admin/subscriptions/AdminSubscriptionsPage'))
const AdminTickets = lazy(() => import('./pages/admin/tickets/AdminTicketsPage'))
const AdminPricing = lazy(() => import('./pages/admin/pricing/AdminPricingPage'))
const AdminRedeemCodes = lazy(() => import('./pages/admin/redeem-codes/AdminRedeemCodesPage'))
const AdminAuditLogs = lazy(() => import('./pages/admin/audit-logs/AdminAuditLogsPage'))

function PageFallback() {
  return (
    <div className="flex min-h-[240px] items-center justify-center text-sm text-muted-foreground">
      页面加载中...
    </div>
  )
}

function eb(children: ReactNode) {
  return <ErrorBoundary>{children}</ErrorBoundary>
}

function App() {
  return (
    <ConfirmModalProvider>
      <Suspense fallback={<PageFallback />}>
        <Routes>
          <Route path="/" element={eb(<Home />)} />

          <Route element={<AuthLayout />}>
            <Route path="/login" element={eb(<Login />)} />
            <Route path="/register" element={eb(<Register />)} />
          </Route>

          <Route element={<UserLayout />}>
            <Route path="/dashboard" element={eb(<Dashboard />)} />
            <Route path="/keys" element={eb(<Keys />)} />
            <Route path="/models" element={eb(<Models />)} />
            <Route path="/usage" element={eb(<Usage />)} />
            <Route path="/balance" element={eb(<BalanceHistory />)} />
            <Route path="/subscriptions" element={eb(<Subscriptions />)} />
            <Route path="/orders" element={eb(<Orders />)} />
            <Route path="/refunds" element={eb(<Refunds />)} />
            <Route path="/refunds/apply" element={eb(<RefundApply />)} />
            <Route path="/tickets" element={eb(<Tickets />)} />
            <Route path="/redeem" element={eb(<Redeem />)} />
            <Route path="/users" element={eb(<AdminUsers />)} />
            <Route path="/channels" element={eb(<AdminChannels />)} />
            <Route path="/billing" element={eb(<AdminBilling />)} />
            <Route path="/usage-logs" element={eb(<AdminUsageLogs />)} />
            <Route path="/settings" element={eb(<AdminSettings />)} />
            <Route path="/payment-config" element={eb(<AdminPaymentConfig />)} />
            <Route path="/order-management" element={eb(<AdminOrders />)} />
            <Route path="/admin/refunds" element={eb(<AdminRefunds />)} />
            <Route path="/admin/subscriptions" element={eb(<AdminSubscriptions />)} />
            <Route path="/admin/tickets" element={eb(<AdminTickets />)} />
            <Route path="/admin/pricing" element={eb(<AdminPricing />)} />
            <Route path="/admin/redeem-codes" element={eb(<AdminRedeemCodes />)} />
            <Route path="/admin/audit-logs" element={eb(<AdminAuditLogs />)} />
          </Route>

          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Suspense>
      <Toaster position="top-center" richColors theme="system" />
    </ConfirmModalProvider>
  )
}

export default App
