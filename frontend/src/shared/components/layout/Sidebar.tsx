import { Link, useLocation, useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/features/auth/auth_store'
import { useAppStore } from '@/shared/store/app_store'
import {
  LayoutDashboard, Key, LogOut,
  Settings, PanelLeftClose, PanelLeft,
  Sun, Moon, Cpu, FileBarChart, ScrollText, Crown,
  ShoppingCart, Ticket, RotateCcw, Gift,
  Users, Network, CreditCard, BarChart3, Wallet,
  ClipboardList, Tag, ShieldAlert,
} from 'lucide-react'
import { useState } from 'react'
import type { LucideIcon } from 'lucide-react'
import { cn } from '@/shared/utils/utils'

type NavLinkItem = {
  label: string
  path: string
  icon: LucideIcon
  /** 侧栏收起时 `title` 提示（缩写项用完整说法） */
  hint?: string
}

export function Sidebar() {
  const logout = useAuthStore(s => s.logout)
  const user = useAuthStore(s => s.user)
  const sidebarCollapsed = useAppStore(s => s.sidebarCollapsed)
  const mobileOpen = useAppStore(s => s.mobileOpen)
  const setMobileOpen = useAppStore(s => s.setMobileOpen)
  const toggleSidebar = useAppStore(s => s.toggleSidebar)
  const location = useLocation()
  const navigate = useNavigate()
  
  const [theme, setTheme] = useState<'light'|'dark'>(
    () => document.documentElement.classList.contains('dark') ? 'dark' : 'light'
  )
  const toggleTheme = () => {
    const isDark = theme === 'dark'
    document.documentElement.classList.toggle('dark', !isDark)
    setTheme(!isDark ? 'dark' : 'light')
    localStorage.setItem('theme', !isDark ? 'dark' : 'light')
  }

  const panelUserNavs: NavLinkItem[] = [
    { label: '总览', path: '/dashboard', icon: LayoutDashboard },
    { label: '密钥', path: '/keys', icon: Key, hint: 'API 密钥' },
    { label: '模型', path: '/models', icon: Cpu },
    { label: '订阅', path: '/subscriptions', icon: Crown },
    { label: '订单', path: '/orders', icon: ShoppingCart },
    { label: '用量', path: '/usage', icon: FileBarChart, hint: '使用明细' },
    { label: '工单', path: '/tickets', icon: Ticket },
    { label: '退款', path: '/refunds', icon: RotateCcw },
    { label: '流水', path: '/balance', icon: ScrollText, hint: '余额流水' },
    { label: '兑换', path: '/redeem', icon: Gift },
  ]

  const adminNavs: NavLinkItem[] = user?.role === 'admin'
    ? [
        { label: '用户', path: '/users', icon: Users, hint: '用户管理' },
        { label: '渠道', path: '/channels', icon: Network, hint: '渠道管理' },
        { label: '计费', path: '/billing', icon: CreditCard, hint: '计费管理' },
        { label: '用量', path: '/usage-logs', icon: BarChart3, hint: '用量日志' },
        { label: '设置', path: '/settings', icon: Settings, hint: '系统设置' },
        { label: '支付', path: '/payment-config', icon: Wallet, hint: '支付配置' },
        { label: '订单', path: '/order-management', icon: ClipboardList, hint: '订单管理' },
        { label: '退款', path: '/admin/refunds', icon: RotateCcw, hint: '退款管理' },
        { label: '订阅', path: '/admin/subscriptions', icon: Crown, hint: '订阅管理' },
        { label: '工单', path: '/admin/tickets', icon: Ticket, hint: '工单管理' },
        { label: '定价', path: '/admin/pricing', icon: Tag, hint: '定价管理' },
        { label: '兑换', path: '/admin/redeem-codes', icon: Gift, hint: '兑换码' },
        { label: '审计', path: '/admin/audit-logs', icon: ShieldAlert, hint: '审计日志' },
      ]
    : []

  const handleLinkClick = () => {
    if (mobileOpen) setMobileOpen(false)
  }

  const isActive = (path: string) => {
    if (path === '/dashboard') return location.pathname === '/dashboard' || location.pathname === '/'
    if (path === '/tickets') {
      return (
        location.pathname === '/tickets' ||
        location.pathname.startsWith('/tickets/')
      )
    }
    return location.pathname === path || location.pathname.startsWith(path + '?') || location.pathname.startsWith(path + '/')
  }

  const navRowClass = (active: boolean) =>
    cn(
      'flex w-full items-center gap-2.5 rounded-xl px-2.5 py-2 text-sm font-medium transition-all duration-200 group',
      active
        ? 'bg-primary-50 dark:bg-primary-900/20 text-primary-600 dark:text-primary-400'
        : 'text-gray-500 dark:text-dark-400 hover:bg-gray-50 dark:hover:bg-dark-800 hover:text-gray-900 dark:hover:text-white'
    )

  const navIconClass = (active: boolean) =>
    cn(
      'h-[18px] w-[18px] flex-shrink-0 transition-colors',
      active
        ? 'text-primary-500'
        : 'text-gray-400 dark:text-dark-500 group-hover:text-gray-600 dark:group-hover:text-dark-300'
    )

  return (
    <>
      <aside 
        className={`fixed inset-y-0 left-0 z-40 flex flex-col bg-white dark:bg-dark-900 border-r border-border transition-all duration-300 ${
          sidebarCollapsed ? 'w-[72px]' : 'w-48'
        } ${!mobileOpen ? '-translate-x-full lg:translate-x-0' : 'translate-x-0'}`}
      >
        {/* Brand Header */}
        <div className="h-16 flex items-center px-3 border-b border-border flex-shrink-0 gap-2.5 max-w-full overflow-hidden">
          <div className="flex w-9 h-9 items-center justify-center rounded-xl bg-gradient-to-br from-primary-500 to-primary-600 text-white shadow-glow flex-shrink-0">
            <span className="font-bold text-lg">C</span>
          </div>
          {!sidebarCollapsed && (
            <span className="text-base font-bold text-gray-900 dark:text-white truncate">
              CPA Gateway
            </span>
          )}
        </div>

        {/* 导航与管理在同一滚动列内；分隔线仅作分组，不把侧栏切成上下两个独立区域 */}
        <nav className="flex flex-1 flex-col min-h-0 overflow-hidden">
          <div className="flex-1 min-h-0 overflow-y-auto py-3 px-2 scrollbar-hide">
            <div className="space-y-1">
              {!sidebarCollapsed && (
                <div className="px-2 mb-2 text-[11px] font-semibold uppercase tracking-widest text-gray-400 dark:text-dark-400">
                  导航
                </div>
              )}
              {sidebarCollapsed && <div className="h-px bg-border mx-3 mb-3" />}

              {panelUserNavs.map((item) => {
                const active = isActive(item.path)
                return (
                  <Link
                    key={item.path}
                    to={item.path}
                    onClick={handleLinkClick}
                    title={sidebarCollapsed ? (item.hint ?? item.label) : undefined}
                    className={cn(navRowClass(active), sidebarCollapsed && 'py-2 justify-center')}
                  >
                    <item.icon className={navIconClass(active)} />
                    {!sidebarCollapsed && <span className="truncate">{item.label}</span>}
                  </Link>
                )
              })}
            </div>

            {adminNavs.length > 0 && (
              <>
                <div
                  className={cn(
                    'mx-3 my-2 border-t border-gray-200/80 dark:border-dark-800',
                    sidebarCollapsed && 'mx-1'
                  )}
                  role="presentation"
                />
                <div className="space-y-1">
                  {!sidebarCollapsed && (
                    <div className="px-2 mb-2 text-[11px] font-semibold uppercase tracking-widest text-gray-400 dark:text-dark-400">
                      管理
                    </div>
                  )}
                  {sidebarCollapsed && <div className="h-px bg-border mx-3 mb-2" />}
                  {adminNavs.map((item) => {
                    const active = isActive(item.path)
                    return (
                      <Link
                        key={item.path}
                        to={item.path}
                        onClick={handleLinkClick}
                        title={sidebarCollapsed ? (item.hint ?? item.label) : undefined}
                        className={cn(navRowClass(active), sidebarCollapsed && 'py-2 justify-center')}
                      >
                        <item.icon className={navIconClass(active)} />
                        {!sidebarCollapsed && <span className="truncate">{item.label}</span>}
                      </Link>
                    )
                  })}
                </div>
              </>
            )}
          </div>
        </nav>

        {/* Footer actions */}
        <div className="p-2 border-t border-border flex flex-col gap-0.5">
          <button
            onClick={toggleTheme}
            title={sidebarCollapsed ? (theme === 'dark' ? '亮色模式' : '暗色模式') : undefined}
            className="flex items-center gap-2.5 rounded-xl px-2.5 py-2 text-sm font-medium text-gray-500 dark:text-dark-400 hover:bg-gray-50 dark:hover:bg-dark-800 hover:text-gray-700 dark:hover:text-white transition-colors w-full"
          >
            {theme === 'dark' ? <Sun className="h-[18px] w-[18px] text-amber-500" /> : <Moon className="h-[18px] w-[18px]" />}
            {!sidebarCollapsed && <span>{theme === 'dark' ? '亮色' : '暗色'}</span>}
          </button>
          
          <button
            onClick={toggleSidebar}
            title={sidebarCollapsed ? "展开侧边栏" : "收起侧边栏"}
            className="hidden lg:flex items-center gap-2.5 rounded-xl px-2.5 py-2 text-sm font-medium text-gray-500 dark:text-dark-400 hover:bg-gray-50 dark:hover:bg-dark-800 hover:text-gray-700 dark:hover:text-white transition-colors w-full"
          >
            {sidebarCollapsed ? <PanelLeft className="h-[18px] w-[18px]" /> : <PanelLeftClose className="h-[18px] w-[18px]" />}
            {!sidebarCollapsed && <span>收起</span>}
          </button>

          <button
            onClick={() => { logout(); navigate('/login') }}
            title={sidebarCollapsed ? "退出登录" : undefined}
            className="flex items-center gap-2.5 rounded-xl px-2.5 py-2 text-sm font-medium text-gray-400 dark:text-dark-500 hover:bg-red-50 dark:hover:bg-red-900/20 hover:text-red-600 dark:hover:text-red-400 transition-colors w-full"
          >
            <LogOut className="h-[18px] w-[18px] flex-shrink-0" />
            {!sidebarCollapsed && <span>退出</span>}
          </button>
        </div>
      </aside>

      {/* Mobile Overlay */}
      {mobileOpen && (
        <div 
          className="fixed inset-0 z-30 bg-black/50 backdrop-blur-sm lg:hidden transition-opacity" 
          onClick={() => setMobileOpen(false)}
        />
      )}
    </>
  )
}
