import { memo, useCallback, useEffect, useState } from 'react'
import { Bell, Menu, ShieldCheck, TicketIcon, User, Wallet } from 'lucide-react'
import { cn } from '@/shared/utils/utils'
import { useNavigate } from 'react-router-dom'
import { useAppStore } from '@/shared/store/app_store'
import { useAuthStore } from '@/features/auth/auth_store'
import { useProfile } from '@/features/auth/hooks'
import { apiClient } from '@/shared/api/client'
import { Button } from '@/shared/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuItem,
} from '@/shared/components/ui/dropdown-menu'

type NotificationItem = {
  id: number
  title: string
  content: string
  is_read: boolean
  notification_type: string
  created_at: string
  related_id?: number | null
}

type NotificationsPage = {
  items: NotificationItem[]
  total: number
}

const NOTIFICATIONS_PAGE_SIZE = 10

function unreadOnPage(items: NotificationItem[], total: number): number | null {
  if (total <= NOTIFICATIONS_PAGE_SIZE) {
    return items.filter((item) => !item.is_read).length
  }
  return null
}

export const Header = memo(function Header() {
  const navigate = useNavigate()
  const setMobileOpen = useAppStore(s => s.setMobileOpen)
  const user = useAuthStore(s => s.user)

  // Subscribe directly to the profile query so the displayed balance reacts
  // immediately whenever any feature invalidates `queryKeys.auth.profile()`
  // (subscription purchase, redeem, top-up, etc.) or the 30s stale window
  // / window-focus refetch fires. Falls back to the auth store cache when
  // the profile query hasn't loaded yet.
  const { data: profile } = useProfile()
  const displayBalance =
    profile?.available_balance ?? (typeof user?.balance === 'number' ? user.balance : undefined)

  const isAdmin = user?.role === 'admin'

  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [items, setItems] = useState<NotificationItem[]>([])
  const [unreadCount, setUnreadCount] = useState(0)

  const loadUnreadCount = useCallback(async () => {
    if (!user) return
    try {
      const data = await apiClient.get<{ unread_count: number }>(
        '/user/notifications/unread-count',
        { cache: 'no-store' }
      )
      setUnreadCount(data.unread_count ?? 0)
    } catch {
      // silently ignore
    }
  }, [user])

  const loadNotifications = useCallback(async () => {
    if (!user) return
    setLoading(true)
    try {
      const data = await apiClient.get<NotificationsPage>(
        `/user/notifications?page=1&page_size=${NOTIFICATIONS_PAGE_SIZE}`,
        { cache: 'no-store' }
      )
      const nextItems = data.items ?? []
      setItems(nextItems)
      const synced = unreadOnPage(nextItems, data.total ?? nextItems.length)
      if (synced !== null) {
        setUnreadCount(synced)
      }
    } catch {
      setItems([])
    } finally {
      setLoading(false)
    }
  }, [user])

  const markRead = useCallback(async (id: number) => {
    setItems((prev) =>
      prev.map((item) => (item.id === id ? { ...item, is_read: true } : item))
    )
    setUnreadCount((prev) => Math.max(0, prev - 1))
    try {
      await apiClient.put(`/user/notifications/${id}/read`)
      await Promise.all([loadUnreadCount(), loadNotifications()])
    } catch {
      await Promise.all([loadUnreadCount(), loadNotifications()])
    }
  }, [loadUnreadCount, loadNotifications])

  const markAllRead = useCallback(async () => {
    setItems((prev) => prev.map((item) => ({ ...item, is_read: true })))
    setUnreadCount(0)
    try {
      await apiClient.put('/user/notifications/read-all')
      await Promise.all([loadUnreadCount(), loadNotifications()])
    } catch {
      await Promise.all([loadUnreadCount(), loadNotifications()])
    }
  }, [loadUnreadCount, loadNotifications])

  useEffect(() => {
    if (user) {
      loadUnreadCount()
    }
  }, [user, loadUnreadCount])

  useEffect(() => {
    if (open) {
      loadNotifications()
      return
    }
    if (user) {
      loadUnreadCount()
    }
  }, [open, user, loadNotifications, loadUnreadCount])

  return (
    <header className="h-16 flex items-center justify-between px-4 lg:px-8 border-b border-border bg-white/80 dark:bg-dark-900/80 backdrop-blur-xl sticky top-0 z-20 shadow-sm">
      <div className="flex items-center gap-3">
        <button
          onClick={() => setMobileOpen(true)}
          className="lg:hidden p-2 -ml-2 rounded-xl text-gray-600 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-dark-800"
        >
          <Menu className="w-5 h-5" />
        </button>

        {/* Mobile Title */}
        <div className="lg:hidden flex items-center gap-2">
          <span className="w-6 h-6 rounded-lg bg-gradient-to-br from-primary-500 to-primary-600 text-white flex items-center justify-center text-xs font-bold">C</span>
          <span className="font-bold text-gray-900 dark:text-white">CPA Gateway</span>
        </div>
      </div>

      <div className="flex items-center gap-4">
        {user && (
          <div className="flex items-center gap-3">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => navigate('/tickets')}
              title="工单"
              className="rounded-xl gap-1.5 text-gray-600 dark:text-gray-300 hover:text-primary-600 dark:hover:text-primary-400"
            >
              <TicketIcon className="w-5 h-5 sm:w-4 sm:h-4 shrink-0" />
              <span className="hidden sm:inline text-sm font-medium">工单</span>
            </Button>

            {/* Notification Bell */}
            <DropdownMenu open={open} onOpenChange={setOpen}>
              <DropdownMenuTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="relative rounded-xl"
                >
                  <Bell className="w-5 h-5 text-gray-600 dark:text-gray-300" />
                  {unreadCount > 0 && (
                    <span className="absolute -top-0.5 -right-0.5 min-w-[18px] h-[18px] px-1 rounded-full bg-red-500 text-white text-[10px] font-bold flex items-center justify-center leading-none">
                      {unreadCount > 99 ? '99+' : unreadCount}
                    </span>
                  )}
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-80">
                <DropdownMenuLabel className="flex items-center justify-between">
                  <span>通知</span>
                  {unreadCount > 0 && (
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-auto py-0.5 px-2 text-xs font-normal text-primary hover:text-primary"
                      onPointerDown={(e) => e.preventDefault()}
                      onClick={(e) => {
                        e.preventDefault()
                        void markAllRead()
                      }}
                    >
                      全部已读
                    </Button>
                  )}
                </DropdownMenuLabel>
                <DropdownMenuSeparator />

                {loading ? (
                  <div className="px-2 py-4 text-center text-sm text-muted-foreground">
                    加载中...
                  </div>
                ) : items.length === 0 ? (
                  <div className="px-2 py-4 text-center text-sm text-muted-foreground">
                    暂无通知
                  </div>
                ) : (
                  items.map(item => (
                    <DropdownMenuItem
                      key={item.id}
                      onSelect={(e) => e.preventDefault()}
                      className={`flex flex-col items-start gap-1 cursor-default ${!item.is_read ? 'bg-accent/50' : ''}`}
                    >
                      <div className="flex items-start justify-between w-full gap-2">
                        <span className={`text-sm font-medium ${!item.is_read ? 'text-foreground' : 'text-muted-foreground'}`}>
                          {item.title}
                        </span>
                        {!item.is_read && (
                          <Button
                            variant="ghost"
                            size="sm"
                            className="h-auto py-0.5 px-2 text-xs font-normal shrink-0"
                            onPointerDown={(e) => e.preventDefault()}
                            onClick={(e) => {
                              e.preventDefault()
                              e.stopPropagation()
                              void markRead(item.id)
                            }}
                          >
                            标记已读
                          </Button>
                        )}
                      </div>
                      <span className="text-xs text-muted-foreground line-clamp-2">
                        {item.content}
                      </span>
                    </DropdownMenuItem>
                  ))
                )}
              </DropdownMenuContent>
            </DropdownMenu>

            {/* Balance Card */}
            {displayBalance !== undefined && (
              <div className="hidden sm:flex items-center gap-2 px-3 py-1.5 bg-gradient-to-r from-emerald-50 to-teal-50 dark:from-emerald-950/30 dark:to-teal-950/30 rounded-lg border border-emerald-100 dark:border-emerald-800/30 mr-2">
                <Wallet className="w-4 h-4 text-emerald-600 dark:text-emerald-400" />
                <div className="flex flex-col items-start leading-none">
                  <span className="text-[10px] uppercase font-bold text-emerald-600/70 dark:text-emerald-400/70 tracking-wider mb-0.5">余额</span>
                  <span className="text-sm font-bold text-emerald-700 dark:text-emerald-300">
                    ${Number(displayBalance).toFixed(4)}
                  </span>
                </div>
              </div>
            )}

            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <button
                  type="button"
                  title={user.email}
                  aria-label={isAdmin ? '管理员账户' : '用户账户'}
                  className={cn(
                    'w-9 h-9 rounded-full flex items-center justify-center shadow-sm transition-colors',
                    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2',
                    isAdmin
                      ? 'bg-emerald-100 dark:bg-emerald-900/30 border border-emerald-200 dark:border-emerald-800 text-emerald-600 dark:text-emerald-400 hover:bg-emerald-200/80 dark:hover:bg-emerald-900/50 focus-visible:ring-emerald-500'
                      : 'bg-primary-100 dark:bg-primary-900/30 border border-primary-200 dark:border-primary-800 text-primary-600 dark:text-primary-400 hover:bg-primary-200/80 dark:hover:bg-primary-900/50 focus-visible:ring-primary-500'
                  )}
                >
                  {isAdmin ? (
                    <ShieldCheck className="w-4 h-4" aria-hidden />
                  ) : (
                    <User className="w-4 h-4" aria-hidden />
                  )}
                </button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-56">
                <DropdownMenuLabel className="font-normal space-y-1">
                  <div className="flex items-center gap-2">
                    {isAdmin ? (
                      <ShieldCheck className="h-4 w-4 text-emerald-600 dark:text-emerald-400 shrink-0" aria-hidden />
                    ) : (
                      <User className="h-4 w-4 text-primary-600 dark:text-primary-400 shrink-0" aria-hidden />
                    )}
                    <p className="text-xs font-semibold text-muted-foreground">
                      {isAdmin ? '管理员' : '用户'}
                    </p>
                  </div>
                  <p className="text-sm font-medium text-foreground break-all">
                    {user.email}
                  </p>
                </DropdownMenuLabel>
              </DropdownMenuContent>
            </DropdownMenu>
          </div>
        )}
      </div>
    </header>
  )
})
