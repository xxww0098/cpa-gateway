import { memo, useCallback, useEffect, useState } from 'react'
import { Bell, Menu, TicketIcon, User, Wallet } from 'lucide-react'
import { useNavigate } from 'react-router-dom'
import { useAppStore } from '@/shared/store/app_store'
import { useAuthStore } from '@/features/auth/auth_store'
import { fetchApi } from '@/shared/api/client'
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

export const Header = memo(function Header() {
  const navigate = useNavigate()
  const setMobileOpen = useAppStore(s => s.setMobileOpen)
  const user = useAuthStore(s => s.user)

  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [items, setItems] = useState<NotificationItem[]>([])
  const [unreadCount, setUnreadCount] = useState(0)

  const loadUnreadCount = useCallback(async () => {
    if (!user) return
    try {
      const res = await fetchApi('/user/notifications/unread-count')
      setUnreadCount(res.data?.unread_count ?? 0)
    } catch {
      // silently ignore
    }
  }, [user])

  const loadNotifications = useCallback(async () => {
    if (!user) return
    setLoading(true)
    try {
      const res = await fetchApi('/user/notifications?page=1&page_size=10')
      setItems(res.data?.items ?? [])
    } catch {
      setItems([])
    } finally {
      setLoading(false)
    }
  }, [user])

  const markRead = useCallback(async (id: number) => {
    try {
      await fetchApi(`/user/notifications/${id}/read`, { method: 'PUT' })
      await loadUnreadCount()
      await loadNotifications()
    } catch {
      // silently ignore
    }
  }, [loadUnreadCount, loadNotifications])

  const markAllRead = useCallback(async () => {
    try {
      await fetchApi('/user/notifications/read-all', { method: 'PUT' })
      await loadUnreadCount()
      await loadNotifications()
    } catch {
      // silently ignore
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
    }
  }, [open, loadNotifications])

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
                      onClick={markAllRead}
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
                            onClick={(e) => {
                              e.stopPropagation()
                              markRead(item.id)
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
            {user.balance !== undefined && (
              <div className="hidden sm:flex items-center gap-2 px-3 py-1.5 bg-gradient-to-r from-emerald-50 to-teal-50 dark:from-emerald-950/30 dark:to-teal-950/30 rounded-lg border border-emerald-100 dark:border-emerald-800/30 mr-2">
                <Wallet className="w-4 h-4 text-emerald-600 dark:text-emerald-400" />
                <div className="flex flex-col items-start leading-none">
                  <span className="text-[10px] uppercase font-bold text-emerald-600/70 dark:text-emerald-400/70 tracking-wider mb-0.5">余额</span>
                  <span className="text-sm font-bold text-emerald-700 dark:text-emerald-300">
                    ${Number(user.balance).toFixed(4)}
                  </span>
                </div>
              </div>
            )}

            <div className="hidden sm:flex flex-col items-end">
              <span className="text-xs font-semibold text-gray-500 dark:text-dark-400 capitalize">
                {user.role === 'admin' ? '管理员' : '用户'}
              </span>
              <span className="text-sm font-medium text-gray-900 dark:text-gray-100">
                {user.email}
              </span>
            </div>
            <div className="w-9 h-9 rounded-full bg-primary-100 dark:bg-primary-900/30 border border-primary-200 dark:border-primary-800 flex items-center justify-center text-primary-600 dark:text-primary-400 shadow-sm">
              <User className="w-4 h-4" />
            </div>
          </div>
        )}
      </div>
    </header>
  )
})
